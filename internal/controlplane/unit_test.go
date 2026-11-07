package controlplane

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/codician-team/truner/internal/agentcfg"
	"github.com/codician-team/truner/internal/config"
	"github.com/codician-team/truner/internal/github"
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
	if strings.Join(r.ExcludedInstanceTypes, " ") != "d2.* d3.* d3en.* h1.* m5* c5* r5* z1d* i3* x1*" || r.LocalStorage != ec2types.LocalStorageIncluded || r.LocalStorageTypes != nil {
		t.Fatalf("defaults must exclude hard disk and old families but keep EBS-only types: %+v", r)
	}
	if old := InstanceRequirements(spec(t, "truner/cpu=8/family=r5a+c7a")); strings.Join(old.AllowedInstanceTypes, " ") != "r5a* c7a*" || old.ExcludedInstanceTypes != nil {
		t.Fatalf("a named old family must stay available: %+v", old)
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

func TestRerunRefused(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&github.Error{Status: 403, Message: "The workflow run containing this job is already running"}, true},
		{&github.Error{Status: 403, Message: "You have exceeded a secondary rate limit"}, false},
		{&github.Error{Status: 404, Message: "Not Found"}, true},
		{&github.Error{Status: 422, Message: "Unprocessable"}, true},
		{&github.Error{Status: 429}, false},
		{&github.Error{Status: 502}, false},
		{errors.New("connection reset"), false},
	} {
		if got := rerunRefused(c.err); got != c.want {
			t.Errorf("%v: got %v", c.err, got)
		}
	}
}

func TestMicroDockerfileHasNodeLibraries(t *testing.T) {
	// Node 26's Arm64 build exits with status 127 without libatomic.so.1.
	if !strings.Contains(microDockerfile, " libatomic1 ") {
		t.Fatal("the MicroVM image must install libatomic1")
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
	if c := jobCost(0.10, 40, 30*time.Minute); c < 0.05 || c > 0.0525 {
		t.Fatalf("cost %v", c)
	}
	if jobCost(0.10, 0, 10*time.Second) != jobCost(0.10, 0, time.Minute) {
		t.Fatal("minimum billing is 60 seconds")
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
