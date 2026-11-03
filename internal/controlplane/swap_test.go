package controlplane

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

const machineTakenLog = "job started on another job's machine before one was launched for it"

// Matrix jobs share labels, so a machine launched for job A may run job B.
// Finishing B must never cause the machine still running A to be reaped.
func TestIntegrationMatrixSwap(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	labels := []string{"truner/cpu=2/run=900"}
	h.webhook("queued", 8001, labels, "", "")
	h.webhook("queued", 8002, labels, "", "")
	h.drainQueue()
	a, b := h.job(8001), h.job(8002)
	machineA, machineB := a.InstanceID, b.InstanceID

	// The machine launched for A runs B, and the one launched for B runs A.
	h.webhook("in_progress", 8002, labels, a.RunnerName, "")
	// A waits for B's machine, which is coming: nothing is launched for it.
	insts, _ := h.cp.ManagedInstances(ctx)
	if err := h.cp.reconcileJobs(ctx, insts, nil); err != nil {
		t.Fatal(err)
	}
	h.drainQueue()
	if n := len(h.jobFleets()); n != 2 {
		t.Fatalf("a swap needs no third machine, got %d fleet requests", n)
	}
	h.webhook("in_progress", 8001, labels, b.RunnerName, "")
	h.webhook("completed", 8002, labels, a.RunnerName, "success")

	if got := h.job(8001); got.Status != StatusRunning || got.MachineDone == 0 || got.PickedBy != 8002 {
		t.Fatalf("A is still running (on B's machine); A's machine is done: %+v", got)
	}
	if got := h.job(8002); got.Status != StatusCompleted || got.MachineDone != 0 || got.RanOnInstance != machineA {
		t.Fatalf("B finished on A's machine; B's machine is still busy: %+v", got)
	}
	// Let the grace period pass and run the janitor.
	h.cp.Store.UpdateJob(ctx, 8001, map[string]any{"machineDoneAt": time.Now().Add(-time.Hour).Unix()}, nil)
	if _, err := h.cp.reconcileInstances(ctx); err != nil {
		t.Fatal(err)
	}
	if st := h.aws.instances[machineA].state; st != "terminated" {
		t.Fatalf("finished machine should be reaped, state %s", st)
	}
	if st := h.aws.instances[machineB].state; st != "running" {
		t.Fatalf("machine still running job A must not be touched, state %s", st)
	}
	h.webhook("completed", 8001, labels, b.RunnerName, "success")
	if got := h.job(8002); got.MachineDone == 0 {
		t.Fatalf("B's machine should now be done: %+v", got)
	}
	if got := h.job(8001); got.Status != StatusCompleted || got.RanOnInstance != machineB || got.Cost <= 0 {
		t.Fatalf("A should be completed with cost: %+v", got)
	}
}

// Three jobs of one run with the same labels. GitHub delivered astro check's
// queued event 38 s after astro check had started on the machine launched for
// typecheck, and typecheck had taken build's. Build must get a machine again,
// and the job on its first machine must still book that machine's cost and
// end it.
func TestIntegrationLateQueuedEvent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var logs bytes.Buffer
	h.cp.Log = slog.New(slog.NewTextHandler(&logs, nil))
	labels := []string{"truner/cpu=4/run=900"}
	const typecheck, astro, build = 9001, 9002, 9003
	h.webhook("queued", typecheck, labels, "", "")
	h.webhook("queued", build, labels, "", "")
	h.drainQueue()
	tc, b := h.job(typecheck), h.job(build)

	h.webhook("in_progress", astro, labels, tc.RunnerName, "")
	if !strings.Contains(logs.String(), machineTakenLog) {
		t.Fatalf("a job that starts on another job's machine before its queued event must be logged:\n%s", logs.String())
	}
	h.webhook("in_progress", typecheck, labels, b.RunnerName, "")
	h.webhook("queued", astro, labels, "", "")
	h.drainQueue()

	if got := h.job(astro); got.Status != StatusRunning || got.InstanceID != "" {
		t.Fatalf("astro check runs on typecheck's machine and has none of its own: %+v", got)
	}
	nb := h.job(build)
	if nb.Status != StatusLaunched || nb.InstanceID == "" || nb.InstanceID == b.InstanceID || nb.RunnerName == b.RunnerName || nb.PickedBy != 0 {
		t.Fatalf("build must get a machine of its own again: %+v", nb)
	}
	if n := len(h.jobFleets()); n != 3 {
		t.Fatalf("expected a third machine, got %d fleet requests", n)
	}
	kept, err := h.cp.Store.GetMachine(ctx, b.RunnerName)
	if err != nil || kept == nil || kept.InstanceID != b.InstanceID || kept.PickedBy != typecheck || kept.Price != b.Price {
		t.Fatalf("build's first machine, which runs typecheck, must be kept: %+v %v", kept, err)
	}
	// Checking again changes nothing.
	insts, _ := h.cp.ManagedInstances(ctx)
	if err := h.cp.reconcileJobs(ctx, insts, nil); err != nil {
		t.Fatal(err)
	}
	h.drainQueue()
	if n := len(h.jobFleets()); n != 3 {
		t.Fatalf("one machine again is enough, got %d fleet requests", n)
	}

	// Typecheck completes on build's first machine: its cost is booked and that
	// machine, not build's new one, is done.
	h.webhook("completed", typecheck, labels, b.RunnerName, "success")
	if got := h.job(typecheck); got.Status != StatusCompleted || got.RanOnInstance != b.InstanceID || got.Cost <= 0 {
		t.Fatalf("typecheck ran on build's first machine: %+v", got)
	}
	if got, _ := h.cp.Store.GetMachine(ctx, b.RunnerName); got.MachineDone == 0 {
		t.Fatalf("build's first machine must be done: %+v", got)
	}
	if got := h.job(build); got.MachineDone != 0 || got.InstanceID != nb.InstanceID {
		t.Fatalf("build's new machine is not done: %+v", got)
	}
	h.cp.Store.UpdateItem(ctx, MachinePK(b.RunnerName), Item{"machineDoneAt": time.Now().Add(-time.Hour).Unix()})
	if _, err := h.cp.reconcileInstances(ctx); err != nil {
		t.Fatal(err)
	}
	if st := h.aws.instances[b.InstanceID].state; st != "terminated" {
		t.Fatalf("build's first machine must be reaped, state %s", st)
	}
	if st := h.aws.instances[nb.InstanceID].state; st != "running" {
		t.Fatalf("build's new machine must not be touched, state %s", st)
	}

	// Build runs on its new machine.
	h.webhook("in_progress", build, labels, nb.RunnerName, "")
	h.webhook("completed", build, labels, nb.RunnerName, "success")
	if got := h.job(build); got.Status != StatusCompleted || got.RanOnInstance != nb.InstanceID || got.MachineDone == 0 || got.Cost <= 0 {
		t.Fatalf("build completed on its new machine: %+v", got)
	}
	h.webhook("completed", astro, labels, tc.RunnerName, "success")
	if got := h.job(astro); got.Status != StatusCompleted || got.RanOnInstance != tc.InstanceID || got.Cost <= 0 {
		t.Fatalf("astro check completed on typecheck's machine: %+v", got)
	}
}

// A job whose launch was deferred (here: still in the queue) can start on a
// machine launched for another job first. The worker drops its launch, and
// the other job gets a machine again.
func TestIntegrationDeferredLaunchMachineTaken(t *testing.T) {
	h := newHarness(t)
	labels := []string{"truner/cpu=4/run=900"}
	h.webhook("queued", 9101, labels, "", "")
	h.drainQueue()
	first := h.job(9101)

	h.webhook("queued", 9102, labels, "", "")
	var logs bytes.Buffer
	h.cp.Log = slog.New(slog.NewTextHandler(&logs, nil))
	h.webhook("in_progress", 9102, labels, first.RunnerName, "")
	if strings.Contains(logs.String(), machineTakenLog) {
		t.Fatalf("the job's own launch is still queued, nothing to report yet:\n%s", logs.String())
	}
	h.drainQueue()
	if !strings.Contains(logs.String(), machineTakenLog) {
		t.Fatalf("the worker must log the launch it drops:\n%s", logs.String())
	}
	if n := len(h.jobFleets()); n != 2 {
		t.Fatalf("expected the first job's machine and one again for it, got %d fleet requests", n)
	}
	if got := h.job(9101); got.Status != StatusLaunched || got.InstanceID == first.InstanceID || got.PickedBy != 0 {
		t.Fatalf("the first job must get a machine again: %+v", got)
	}
	if got := h.job(9102); got.InstanceID != "" {
		t.Fatalf("the deferred job ran on the first job's machine and needs none: %+v", got)
	}
}

// A job that GitHub no longer has queued (it was cancelled, or started and
// its webhook is on its way) gets no machine again.
func TestIntegrationTakenJobNotQueued(t *testing.T) {
	h := newHarness(t)
	labels := []string{"truner/cpu=4/run=900"}
	h.webhook("queued", 9201, labels, "", "")
	h.drainQueue()
	first := h.job(9201)
	h.gh.jobStatus[9201] = "in_progress"
	h.webhook("in_progress", 9202, labels, first.RunnerName, "")
	h.drainQueue()
	if n := len(h.jobFleets()); n != 1 {
		t.Fatalf("expected no second machine, got %d fleet requests", n)
	}
	if got := h.job(9201); got.InstanceID != first.InstanceID || got.PickedBy != 9202 {
		t.Fatalf("the record must be left as it was: %+v", got)
	}
}
