package costs

import "testing"

func TestSummarize(t *testing.T) {
	s := Summarize([]Usage{
		{Repo: "a/b", Workflow: "CI", Cost: 0.02, CPU: 4, InstanceType: "c7a.xlarge", Lifecycle: "spot", QueuedAt: 100, StartedAt: 130, CompletedAt: 130 + 600},
		{Repo: "a/b", Workflow: "CI", Cost: 0.01, CPU: 2, InstanceType: "c7g.large", Lifecycle: "on-demand", QueuedAt: 100, StartedAt: 110, CompletedAt: 110 + 61},
		{Repo: "a/c", Status: "failed"},
	})
	if s.Jobs != 3 || s.LaunchFailures != 1 || s.ByRepo["a/b"] != 0.03 {
		t.Fatalf("unexpected summary %+v", s)
	}
	// 10 min on 4 x64 cores + 2 min (rounded up) on 2 arm64 cores.
	want := 10*GitHubPerMinute(4, "linux", "x64") + 2*GitHubPerMinute(2, "linux", "arm64")
	if diff := s.GitHubCost - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("github cost %v want %v", s.GitHubCost, want)
	}
	if s.SpotShare != 0.5 || s.MedianWait != 30 {
		t.Fatalf("spot %v wait %v", s.SpotShare, s.MedianWait)
	}
}

func TestArchOf(t *testing.T) {
	for in, want := range map[string]string{"c7g.large": "arm64", "m8g.xlarge": "arm64", "c7gd.2xlarge": "arm64", "c7a.large": "x64", "g5.xlarge": "x64", "m7i-flex.large": "x64", "microvm-4vcpu": "arm64"} {
		if got := archOf(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

func TestGitHubPerMinute(t *testing.T) {
	for _, c := range []struct {
		cpu            int
		platform, arch string
		want           float64
	}{
		{1, "linux", "x64", 0.006},
		{2, "linux", "x64", 0.006},
		{16, "linux", "x64", 0.042},
		{48, "linux", "x64", 0.162}, // no 48-core runner: the next size up
		{4, "linux", "arm64", 0.008},
		{16, "linux", "arm64", 0.026},
		{16, "windows", "x64", 0.082},
		{4, "windows", "arm64", 0.014},
		{192, "linux", "x64", 0.504}, // larger than any runner: pro rata
	} {
		if got := GitHubPerMinute(c.cpu, c.platform, c.arch); got != c.want {
			t.Errorf("%d %s %s: %v, want %v", c.cpu, c.platform, c.arch, got, c.want)
		}
	}
}

func TestMachineCores(t *testing.T) {
	for in, want := range map[string]int{"m6g.medium": 1, "m7a.large": 2, "c8a.xlarge": 4, "c8a.4xlarge": 16, "m8azn.12xlarge": 48,
		"microvm-1vcpu": 4, "microvm-4vcpu": 16, "c7a.metal-48xl": 7, "": 7} {
		if got := machineCores(in, 7); got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
}

// A pinned type= records the label's minimum CPU, and a MicroVM's CPU is its
// baseline. The comparison uses the machine the job really had.
func TestSummarizeComparesTheMachineTheJobHad(t *testing.T) {
	s := Summarize([]Usage{
		{InstanceType: "c8a.4xlarge", CPU: 2, StartedAt: 100, CompletedAt: 700},
		{InstanceType: "microvm-4vcpu", CPU: 4, StartedAt: 100, CompletedAt: 700, Cost: 0.2, Billed: true},
		{InstanceType: "c7a.xlarge", CPU: 4, StartedAt: 100, CompletedAt: 160, Cost: 0.1},
	})
	want := 10*0.042 + 10*0.026 + 1*0.012
	if diff := s.GitHubCost - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("github cost %v want %v", s.GitHubCost, want)
	}
	if s.Billed != 0.2 || s.Estimated != 0.1 {
		t.Fatalf("billed %v estimated %v", s.Billed, s.Estimated)
	}
}
