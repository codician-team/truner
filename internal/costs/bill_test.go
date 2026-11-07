package costs

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestUsageKey(t *testing.T) {
	for in, want := range map[string]string{
		"SpotUsage:c8a.4xlarge":                "SpotUsage:c8a.4xlarge",
		"EUN1-SpotUsage:c5a.2xlarge":           "SpotUsage:c5a.2xlarge",
		"BoxUsage:c7a.4xlarge":                 "BoxUsage:c7a.4xlarge",
		"Lambda-MicroVM-vCPU-Second-ARM":       MicroVMKey,
		"USE2-Lambda-MicroVM-Snapshot-Read-GB": MicroVMKey,
		"EBS:VolumeUsage.gp3":                  "",
		"TimedStorage-ByteHrs":                 "",
	} {
		if got := UsageKey(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
	if BillKey("c8a.4xlarge", "spot") != "SpotUsage:c8a.4xlarge" || BillKey("c7a.xlarge", "on-demand") != "BoxUsage:c7a.xlarge" ||
		BillKey("microvm-1vcpu", "microvm") != MicroVMKey || BillKey("", "spot") != "" {
		t.Fatal("bill keys")
	}
}

// The account also runs its own EC2, of the same type as a job's machine. Only
// the line with this stack's tag may end up on the job.
func TestReconcileLeavesOtherEC2Out(t *testing.T) {
	const k = "SpotUsage:m7a.2xlarge"
	lines := []BillLine{
		{"d1", "truner", k, 0.90},
		{"d1", "", k, 5.00},            // someone's canary workers
		{"d1", "other-stack", k, 7.00}, // another installation
		{"d1", "", "EUN1-SpotUsage:c5a.large", 3},
	}
	jobs := []JobEstimate{
		{Stack: "truner", Day: "d1", Key: k, Machine: 0.60, Overhead: 0.02},
		{Stack: "truner", Day: "d1", Key: k, Machine: 0.40, Overhead: 0.02},
		{Stack: "other-stack", Day: "d1", Key: k, Machine: 8.00},
	}
	d := Reconcile("truner", lines, jobs, true)["d1"]
	if !near(d.Ratios[k], 0.9) || len(d.Ratios) != 1 {
		t.Fatalf("ratios %v", d.Ratios)
	}
	if c, ok := d.Cost(jobs[0]); !ok || !near(c, 0.56) {
		t.Fatalf("cost %v %v", c, ok)
	}
	if !near(d.Billed, 0.94) || !near(d.Estimated, 1.04) {
		t.Fatalf("billed %v estimated %v", d.Billed, d.Estimated)
	}
}

// While the tag is not active, no EC2 line can be told apart. EC2 jobs keep
// their estimate rather than show a number that may contain other work.
func TestReconcileWithoutActiveTagKeepsEC2Estimates(t *testing.T) {
	const k = "SpotUsage:m7a.2xlarge"
	lines := []BillLine{
		{"d1", "", k, 9},
		{"d1", "", "Lambda-MicroVM-vCPU-Second-ARM", 0.3},
		{"d1", "", "Lambda-MicroVM-Snapshot-Read-GB", 0.1},
	}
	ec2 := JobEstimate{Stack: "truner", Day: "d1", Key: k, Machine: 1, Overhead: 0.1}
	micro := JobEstimate{Stack: "truner", Day: "d1", Key: MicroVMKey, Machine: 0.5}
	d := Reconcile("truner", lines, []JobEstimate{ec2, micro}, true)["d1"]
	if c, ok := d.Cost(ec2); ok || !near(c, 1.1) {
		t.Fatalf("EC2 cost %v %v", c, ok)
	}
	if c, ok := d.Cost(micro); !ok || !near(c, 0.4) {
		t.Fatalf("micro cost %v %v", c, ok)
	}
	if !near(d.Estimated, 0.5) || !near(d.Billed, 0.4) {
		t.Fatalf("estimated %v billed %v", d.Estimated, d.Billed)
	}
}

func TestReconcileSharesMicroVMsBetweenStacks(t *testing.T) {
	lines := []BillLine{{"d1", "", "Lambda-MicroVM-vCPU-Second-ARM", 2}}
	a := JobEstimate{Stack: "a", Day: "d1", Key: MicroVMKey, Machine: 3}
	b := JobEstimate{Stack: "b", Day: "d1", Key: MicroVMKey, Machine: 1}
	da := Reconcile("a", lines, []JobEstimate{a, b}, true)["d1"]
	db := Reconcile("b", lines, []JobEstimate{a, b}, true)["d1"]
	if !near(da.Billed, 1.5) || !near(db.Billed, 0.5) {
		t.Fatalf("a %v b %v", da.Billed, db.Billed)
	}
	// Something else runs MicroVMs here: the line cannot be split.
	if d := Reconcile("a", lines, []JobEstimate{a, b}, false)["d1"]; d != nil {
		t.Fatalf("foreign MicroVMs: %+v", d)
	}
}

func TestReconcilePlatformAndGaps(t *testing.T) {
	const k = "BoxUsage:c7a.xlarge"
	lines := []BillLine{
		{"d1", "truner", "TimedStorage-ByteHrs", 0.2},
		{"d1", "truner", "EBS:SnapshotUsage", 0.1},
		{"d1", "truner", "EBS:VolumeUsage.gp3", 0.5}, // in the jobs' overhead
		{"d1", "other", "TimedStorage-ByteHrs", 9},
		{"d1", "", "TimedStorage-ByteHrs", 9},
		{"d2", "truner", k, 0}, // not posted
	}
	job := JobEstimate{Stack: "truner", Day: "d2", Key: k, Machine: 1}
	got := Reconcile("truner", lines, []JobEstimate{job}, true)
	if !near(got["d1"].Platform, 0.3) {
		t.Fatalf("platform %v", got["d1"].Platform)
	}
	if c, ok := got["d2"].Cost(job); ok || c != 1 {
		t.Fatalf("an empty line must keep the estimate: %v %v", c, ok)
	}
	var none *DayBill
	if c, ok := none.Cost(job); ok || c != 1 {
		t.Fatalf("no bill: %v %v", c, ok)
	}
}
