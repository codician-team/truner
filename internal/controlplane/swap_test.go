package controlplane

import (
	"context"
	"testing"
	"time"
)

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
