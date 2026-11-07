package controlplane

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/codician-team/truner/internal/agentcfg"
	"github.com/codician-team/truner/internal/config"
	"github.com/codician-team/truner/internal/labels"
)

func mustGlobal(t *testing.T, y string) *config.Global {
	t.Helper()
	g, err := config.ParseGlobal([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func spec(t *testing.T, raw ...string) *config.JobSpec {
	t.Helper()
	l, _ := labels.Parse("truner", raw)
	s, err := config.Resolver{}.Resolve(l)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestInstanceRequirements(t *testing.T) {
	r := InstanceRequirements(spec(t, "truner/cpu=4-8/ram=16/arch=arm64"))
	if *r.VCpuCount.Min != 4 || *r.VCpuCount.Max != 8 || *r.MemoryMiB.Min != 16384 || r.MemoryMiB.Max != nil {
		t.Fatalf("cpu/ram wrong: %+v %+v", r.VCpuCount, r.MemoryMiB)
	}
	if len(r.CpuManufacturers) != 1 || r.CpuManufacturers[0] != ec2types.CpuManufacturerAmazonWebServices {
		t.Fatalf("arm64 must select Graviton: %v", r.CpuManufacturers)
	}
	if r.BurstablePerformance != ec2types.BurstablePerformanceExcluded || r.AcceleratorCount.Max == nil || *r.AcceleratorCount.Max != 0 {
		t.Fatal("defaults must exclude burstable and accelerated types")
	}
	if strings.Join(r.ExcludedInstanceTypes, " ") != "d2.* d3.* d3en.* h1.*" || r.LocalStorage != ec2types.LocalStorageIncluded || r.LocalStorageTypes != nil {
		t.Fatalf("defaults must exclude hard disk families but keep EBS-only types: %+v", r)
	}
	g := InstanceRequirements(spec(t, "truner/runner=gpu-nvidia"))
	if *g.AcceleratorCount.Min != 1 || g.AcceleratorManufacturers[0] != ec2types.AcceleratorManufacturerNvidia {
		t.Fatalf("gpu requirements wrong: %+v", g)
	}
	f := InstanceRequirements(spec(t, "truner/family=t3+m7a/nvme=true"))
	if f.BurstablePerformance != ec2types.BurstablePerformanceIncluded || len(f.AllowedInstanceTypes) != 2 || f.LocalStorage != ec2types.LocalStorageRequired || f.ExcludedInstanceTypes != nil {
		t.Fatalf("family/nvme wrong (EC2 rejects allowed and excluded types together): %+v", f)
	}
}

func TestBuildOverrides(t *testing.T) {
	o := buildOverrides(spec(t, "truner/type=c7a.large+m7a.large"), []string{"s1", "s2", "s3"})
	if len(o) != 6 || o[0].InstanceRequirements != nil || o[0].InstanceType != "c7a.large" {
		t.Fatalf("explicit types expand per subnet: %+v", o)
	}
	o = buildOverrides(spec(t, "truner"), []string{"s1", "s2"})
	if len(o) != 2 || o[1].InstanceRequirements == nil {
		t.Fatal("attribute-based selection uses one override per subnet")
	}
}

func TestLaunchJobID(t *testing.T) {
	if launchJobID("truner-12345-abcdef") != 12345 || launchJobID("my-runner") != 0 || launchJobID("truner-x") != 0 {
		t.Fatal("launchJobID")
	}
}

func TestUserDataRoundTrip(t *testing.T) {
	c := &agentcfg.Config{Version: "v1", Mode: agentcfg.ModeRun, Platform: "linux", JITConfig: strings.Repeat("J", 3000), Job: agentcfg.JobInfo{Repo: "a/b"}}
	enc, err := agentcfg.Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, ud := range []string{linuxUserData(enc, "https://example/agent?sig=1&x=2", "v1"), windowsUserData(enc, "https://example/agent", "v1")} {
		if len(ud) > 16000 {
			t.Fatalf("user data too large: %d", len(ud))
		}
		got, err := agentcfg.FromUserData([]byte(ud))
		if err != nil {
			t.Fatal(err)
		}
		if got.JITConfig != c.JITConfig || got.Job.Repo != "a/b" {
			t.Fatal("round trip lost data")
		}
	}
	if !strings.HasPrefix(linuxUserData(enc, "u", "v1"), "#!/bin/bash\n") {
		t.Fatal("linux user data must start with a shebang")
	}
}

func TestJobCost(t *testing.T) {
	if c := jobCost(0.10, 0.004, 30*time.Minute); c < 0.0519 || c > 0.0521 {
		t.Fatalf("cost %v", c)
	}
	if jobCost(0.10, 0, 10*time.Second) != jobCost(0.10, 0, time.Minute) {
		t.Fatal("minimum billing is 60 seconds")
	}
}

func TestOverheadPerHour(t *testing.T) {
	near := func(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
	if got := (&Job{DiskGiB: 73}).overheadPerHour(); !near(got, 0.008) {
		t.Fatalf("disk only: %v", got)
	}
	if got := (&Job{DiskGiB: 73, PublicIP: true, DiskIOPS: 3000, DiskMiBps: 125}).overheadPerHour(); !near(got, 0.013) {
		t.Fatalf("included IOPS and throughput cost nothing: %v", got)
	}
	want := (100*0.08 + 5000*0.005 + 875*0.04) / 730
	if got := (&Job{DiskGiB: 100, DiskIOPS: 8000, DiskMiBps: 1000}).overheadPerHour(); !near(got, want) {
		t.Fatalf("provisioned disk: %v, want %v", got, want)
	}
}

// The numbers from the bill of 2026-11-03 to 2026-11-05: a 60 s job on the
// 4 vCPU MicroVM, which is mostly compute, and the short jobs that are mostly
// snapshot reads.
func TestMicroVMCostFollowsTheBill(t *testing.T) {
	const launched = 1000
	j := Job{InstanceID: "microvm-abc", LaunchedAt: launched, CPU: 4, Price: MicroHourlyPrice(4)}
	base := MicroHourlyPrice(4) / 60 // one minute at the baseline
	got := j.machineCost(time.Unix(launched+60, 0))
	if want := base*MicroBurstFactor + (1.18+0.26*4)*0.00155; got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("cost %v, want %v", got, want)
	}
	if MicroStartPrice(1) >= MicroStartPrice(4) || MicroStartPrice(4) > 0.004 {
		t.Fatalf("snapshot reads: %v, %v", MicroStartPrice(1), MicroStartPrice(4))
	}
}

func TestBillEstimate(t *testing.T) {
	const at = 1_762_300_800 // 2025-11-05 00:00 UTC
	spot := Job{LaunchedAt: at, CompletedAt: at + 3600, DiskGiB: 73, PublicIP: true, Cost: 0.20, InstanceType: "c7a.xlarge", Lifecycle: "on-demand", RanOnType: "m7a.xlarge", RanOnLifecycle: "spot"}
	e := spot.BillEstimate("truner")
	if e.Stack != "truner" || e.Day != "2025-11-05" || e.Key != "SpotUsage:m7a.xlarge" {
		t.Fatalf("estimate %+v", e)
	}
	if e.Overhead < 0.0129 || e.Overhead > 0.0131 || e.Machine+e.Overhead != 0.20 {
		t.Fatalf("split %+v", e)
	}
	micro := Job{LaunchedAt: at, MachineDone: at + 60, Cost: 0.05, InstanceID: "microvm-1", InstanceType: "microvm-4vcpu", Lifecycle: "microvm", DiskGiB: 73}
	if e := micro.BillEstimate("truner"); e.Key != "microvm" || e.Overhead != 0 || e.Machine != 0.05 {
		t.Fatalf("micro %+v", e)
	}
	// Overhead never exceeds the estimate, e.g. for a record with a small cost.
	cheap := Job{LaunchedAt: at, UpdatedAt: at + 3600, DiskGiB: 1000, Cost: 0.01, InstanceType: "c7a.xlarge", Lifecycle: "spot"}
	if e := cheap.BillEstimate("truner"); e.Overhead != 0.01 || e.Machine != 0 {
		t.Fatalf("clamped %+v", e)
	}
}

func TestParseOnDemand(t *testing.T) {
	p, ok := parseOnDemand(`{"terms":{"OnDemand":{"a":{"priceDimensions":{"b":{"pricePerUnit":{"USD":"0.0864000000"}}}}}}}`)
	if !ok || p != 0.0864 {
		t.Fatalf("got %v %v", p, ok)
	}
}

func TestRunnerFile(t *testing.T) {
	if runnerFile("2.338.2", "linux", "arm64") != "actions-runner-linux-arm64-2.338.2.tar.gz" || runnerFile("2.338.2", "windows", "x64") != "actions-runner-win-x64-2.338.2.zip" {
		t.Fatal("runner file names")
	}
}

func TestBakedImageVersion(t *testing.T) {
	r := describeToResolved(ec2types.Image{ImageId: aws.String("ami-1"), Tags: []ec2types.Tag{{Key: aws.String(TagVersion), Value: aws.String("v0.1.0")}}}, "ubuntu24")
	if r.Version != "v0.1.0" {
		t.Fatalf("version = %q", r.Version)
	}
}
