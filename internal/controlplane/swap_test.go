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

// bake3d run 41669209801 (2026-11-03): three jobs with the same labels. GitHub
// delivered astro check's queued event 38 s after astro check had started on
// the MicroVM launched for typecheck, typecheck had taken build's, and build
// waited with no machine. The control plane must say so, and keep the records
// `truner doctor` reads to find the waiting job.
func TestIntegrationLateQueuedEvent(t *testing.T) {
	h := newHarness(t)
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

	if got := h.job(build); got.Status != StatusLaunched || got.PickedBy != typecheck || got.StartedAt != 0 {
		t.Fatalf("build's machine ran typecheck and build has not started: %+v", got)
	}
	if got := h.job(astro); got.Status != StatusRunning || got.InstanceID != "" {
		t.Fatalf("astro check runs on typecheck's machine and has none of its own: %+v", got)
	}
}

// A job whose launch was deferred (here: still in the queue) can start on a
// machine launched for another job first; the worker then drops its launch.
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
	if n := len(h.jobFleets()); n != 1 {
		t.Fatalf("expected only the first job's machine, got %d fleet requests", n)
	}
}
