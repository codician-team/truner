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
	for in, want := range map[string]string{"c7g.large": "arm64", "m8g.xlarge": "arm64", "c7gd.2xlarge": "arm64", "c7a.large": "x64", "g5.xlarge": "x64", "m7i-flex.large": "x64"} {
		if got := archOf(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}
