package costs

import "strings"

// MicroVMKey is the bill key of all MicroVM usage. Lambda bills MicroVMs by
// region only: RunMicrovm takes no tags.
const MicroVMKey = "microvm"

// BillKey connects a job's machine to the lines of the AWS bill.
func BillKey(instanceType, lifecycle string) string {
	switch {
	case instanceType == "":
		return ""
	case strings.HasPrefix(instanceType, "microvm-"):
		return MicroVMKey
	case lifecycle == "spot":
		return "SpotUsage:" + instanceType
	}
	return "BoxUsage:" + instanceType
}

// UsageKey returns the bill key of a Cost Explorer usage type, without its
// region prefix (EUN1-SpotUsage:c5a.2xlarge), or "" for anything but a machine.
func UsageKey(usage string) string {
	if strings.Contains(usage, "Lambda-MicroVM") {
		return MicroVMKey
	}
	for _, p := range []string{"SpotUsage:", "BoxUsage:"} {
		if i := strings.Index(usage, p); i >= 0 {
			return usage[i:]
		}
	}
	return ""
}

// BillLine is one line of the AWS bill. Stack is the value of its truner:stack
// tag: empty for anything that is not truner's, for everything while the tag
// is not an active cost allocation tag, and for MicroVMs.
type BillLine struct {
	Day, Stack, Usage string
	Cost              float64
}

// JobEstimate is a job's estimate, split into what the bill's machine lines
// cover and what they do not (its disk and public IP, at list price).
type JobEstimate struct {
	Stack, Day, Key   string
	Machine, Overhead float64
}

// DayBill is what the AWS bill says about one stack's jobs on one day.
type DayBill struct {
	// Ratios holds billed/estimated per bill key. A key without a ratio has
	// no bill line that is certainly truner's, and its jobs keep their estimate.
	Ratios    map[string]float64 `json:"ratios,omitempty"`
	Estimated float64            `json:"estimated,omitempty"` // estimates of the jobs the bill covers
	Billed    float64            `json:"billed,omitempty"`    // what those jobs cost on the bill
	Platform  float64            `json:"platform,omitempty"`  // this stack's storage, images and control plane
}

// Cost returns a job's cost: its share of the bill when there is one, otherwise
// its estimate.
func (d *DayBill) Cost(e JobEstimate) (cost float64, billed bool) {
	if d != nil {
		if r, ok := d.Ratios[e.Key]; ok {
			return e.Machine*r + e.Overhead, true
		}
	}
	return e.Machine + e.Overhead, false
}

// Reconcile compares one stack's jobs with the bill, per day and bill key.
// jobs holds the jobs of every truner stack in the region.
//
// EC2 lines count only when they carry this stack's tag, so other EC2 in the
// account, even of the same instance type, never ends up on a job. MicroVM
// lines cannot be tagged: they are split between all stacks' micro jobs, and
// microVMsOK says that nothing else in the region runs MicroVMs.
func Reconcile(stack string, lines []BillLine, jobs []JobEstimate, microVMsOK bool) map[string]*DayBill {
	type dayKey struct{ day, key string }
	billed := map[dayKey]float64{}
	platform := map[string]float64{}
	for _, l := range lines {
		key := UsageKey(l.Usage)
		switch {
		case key == MicroVMKey:
			if microVMsOK {
				billed[dayKey{l.Day, key}] += l.Cost
			}
		case l.Stack != stack:
		case key != "":
			billed[dayKey{l.Day, key}] += l.Cost
		case !strings.Contains(l.Usage, "EBS:Volume"): // job disks are in the jobs' overhead
			platform[l.Day] += l.Cost
		}
	}
	estimated := map[dayKey]float64{}
	for _, j := range jobs {
		// Only MicroVMs share a bill line between stacks.
		if j.Stack == stack || j.Key == MicroVMKey {
			estimated[dayKey{j.Day, j.Key}] += j.Machine
		}
	}
	out := map[string]*DayBill{}
	day := func(d string) *DayBill {
		if out[d] == nil {
			out[d] = &DayBill{Ratios: map[string]float64{}}
		}
		return out[d]
	}
	for k, b := range billed {
		if e := estimated[k]; e > 0 && b > 0 {
			day(k.day).Ratios[k.key] = b / e
		}
	}
	for d, p := range platform {
		day(d).Platform = p
	}
	for _, j := range jobs {
		if j.Stack != stack || out[j.Day] == nil {
			continue
		}
		if cost, ok := out[j.Day].Cost(j); ok {
			out[j.Day].Estimated += j.Machine + j.Overhead
			out[j.Day].Billed += cost
		}
	}
	return out
}
