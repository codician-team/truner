package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestHoldsMicroVM(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	launched := now.Add(-time.Minute).Unix()
	for _, c := range []struct {
		name string
		j    Job
		want bool
	}{
		{"launched", Job{InstanceID: "mvm-1", Status: StatusLaunched, LaunchedAt: launched}, true},
		{"running", Job{InstanceID: "mvm-1", Status: StatusRunning, LaunchedAt: launched}, true},
		{"EC2 machine", Job{InstanceID: "i-1", Status: StatusRunning, LaunchedAt: launched}, false},
		{"no machine", Job{Status: StatusQueued}, false},
		{"its job is over", Job{InstanceID: "mvm-1", Status: StatusCompleted, LaunchedAt: launched, MachineDone: now.Unix()}, false},
		{"cancelled", Job{InstanceID: "mvm-1", Status: StatusCancelled, LaunchedAt: launched}, false},
		{"cancelled, another job runs on it", Job{InstanceID: "mvm-1", Status: StatusCancelled, LaunchedAt: launched, PickedBy: 2}, true},
		{"failed", Job{InstanceID: "mvm-1", Status: StatusFailed, LaunchedAt: launched}, false},
		{"its job ran elsewhere", Job{InstanceID: "mvm-1", Status: StatusCompleted, LaunchedAt: launched}, true},
		{"older than a MicroVM lives", Job{InstanceID: "mvm-1", Status: StatusRunning, LaunchedAt: now.Add(-microMaxLife).Unix()}, false},
	} {
		if got := c.j.holdsMicroVM(now); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestMicroListWait(t *testing.T) {
	for n, want := range map[int]time.Duration{1: 4 * time.Minute, 2: 8 * time.Minute, 3: 16 * time.Minute, 9: 16 * time.Minute} {
		if got := microListWait(n, 0); got != want {
			t.Errorf("throttled %d times: waits %s, want %s", n, got, want)
		}
	}
	if got := microListWait(1, time.Hour); got != time.Hour {
		t.Errorf("AWS's retry-after wins when it is longer: %s", got)
	}
}

func (h *harness) readyMicroImage(vcpu int) {
	h.t.Helper()
	name := h.cp.microImageName(vcpu)
	arn := "arn:aws:lambda:us-east-1:123456789012:microvm-image:" + name
	err := h.cp.Store.UpdateItem(context.Background(), microPK(vcpu), Item{"coll": collMicro, "ts": time.Now().Unix(), "vcpu": vcpu, "status": "ready",
		"arn": arn, "imageVersion": "1", "agent": "v-test", "runner": "2.338.2"})
	if err != nil {
		h.t.Fatal(err)
	}
	h.aws.microImages[name] = &fakeMicroImage{name: name, arn: arn, versions: map[string]string{"1": "SUCCESSFUL"}}
}

// ListMicrovms is called while a record holds a MicroVM and once in a while,
// not for launches.
func TestIntegrationMicroListOnlyWhenNeeded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	janitor := func(wantLists, wantUp int) {
		t.Helper()
		live, err := h.cp.janitorMicrovms(ctx)
		if err != nil || live == nil || len(live) != wantUp || h.aws.microLists != wantLists {
			t.Fatalf("janitor: live %v, err %v, %d lists; want %d lists and %d up", live, err, h.aws.microLists, wantLists, wantUp)
		}
	}

	// No MicroVM image: nothing to ask Lambda.
	janitor(0, 0)
	h.readyMicroImage(2)
	// The sweep for MicroVMs no record knows about, then none until the next.
	janitor(1, 0)
	if h.aws.microListSize != "50" {
		t.Fatalf("the list must ask for the largest page, got maxResults=%q", h.aws.microListSize)
	}
	janitor(1, 0)

	// Launching does not list, on EC2 or on a MicroVM.
	labels := []string{"truner/micro/run=900"}
	h.webhook("queued", 9601, []string{"truner/cpu=2/run=900"}, "", "")
	h.webhook("queued", 9602, labels, "", "")
	h.drainQueue()
	j := h.job(9602)
	if !isMicroVM(j.InstanceID) || h.aws.microLists != 1 {
		t.Fatalf("expected a MicroVM launched without a list: %+v, %d lists", j, h.aws.microLists)
	}
	if err := h.cp.checkTaken(ctx, 9602); err != nil || h.aws.microLists != 1 {
		t.Fatalf("the check after a swap must not list: %v, %d lists", err, h.aws.microLists)
	}
	insts, _ := h.cp.ManagedInstances(ctx, "pending", "running") // the job's machine and an image build
	h.cp.countAt = time.Time{}
	if n, err := h.cp.runningCount(ctx); err != nil || n != len(insts)+1 || h.aws.microLists != 1 {
		t.Fatalf("expected %d machines and the MicroVM counted without a list: %d %v, %d lists", len(insts), n, err, h.aws.microLists)
	}

	// With a MicroVM up, every janitor run lists.
	janitor(2, 1)
	janitor(3, 1)
	// Once its job is done, they stop.
	h.webhook("in_progress", 9602, labels, j.RunnerName, "")
	h.webhook("completed", 9602, labels, j.RunnerName, "success")
	janitor(3, 0)
}

// A throttled list pauses the janitor's list, and the MicroVMs still count
// toward the instance limit.
func TestIntegrationMicroListThrottled(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.readyMicroImage(2)
	h.cp.mu.Lock()
	h.cp.global, h.cp.globalAt = mustGlobal(t, "limits:\n  maxInstances: 1\n"), time.Now()
	h.cp.mu.Unlock()
	labels := []string{"truner/micro/run=900"}
	h.webhook("queued", 9701, labels, "", "")
	h.drainQueue()
	if !isMicroVM(h.job(9701).InstanceID) {
		t.Fatalf("expected a MicroVM: %+v", h.job(9701))
	}

	h.aws.microListFail = "ThrottlingException"
	live, err := h.cp.janitorMicrovms(ctx)
	if err == nil || live != nil || !strings.Contains(err.Error(), "ThrottlingException") {
		t.Fatalf("a throttled list reports the error and unknown liveness: %v %v", live, err)
	}
	it, _ := h.cp.Store.GetItem(ctx, microBackoffPK)
	if it == nil || it.Int("n") != 1 || it.Int("until") < time.Now().Add(3*time.Minute).Unix() {
		t.Fatalf("expected a back-off of about 4 minutes: %v", it)
	}
	lists := h.aws.microLists
	live, err = h.cp.janitorMicrovms(ctx)
	if err != nil || live != nil || h.aws.microLists != lists {
		t.Fatalf("while backing off, liveness is unknown and nothing is listed: %v %v, %d lists", live, err, h.aws.microLists-lists)
	}

	// The MicroVM still counts: the next job waits for the limit.
	h.cp.countAt = time.Time{}
	h.webhook("queued", 9702, labels, "", "")
	h.drainQueue()
	if j := h.job(9702); j.Status != StatusQueued || !strings.Contains(j.Error, "instance limit") || h.aws.microLists != lists {
		t.Fatalf("the instance limit must count the MicroVM without a list: %+v", j)
	}

	// Once the wait is over and the list works, the back-off is forgotten.
	h.aws.microListFail = ""
	h.cp.Store.UpdateItem(ctx, microBackoffPK, Item{"until": time.Now().Add(-time.Second).Unix()})
	live, err = h.cp.janitorMicrovms(ctx)
	if err != nil || len(live) != 1 {
		t.Fatalf("expected the MicroVM listed: %v %v", live, err)
	}
	if it, _ := h.cp.Store.GetItem(ctx, microBackoffPK); it != nil {
		t.Fatalf("the back-off must be cleared: %v", it)
	}
}
