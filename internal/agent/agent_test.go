package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codician-team/truner/internal/agentcfg"
	"github.com/codician-team/truner/internal/cacheproxy"
)

func TestPatchWorker(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	dll := filepath.Join(dir, "bin", "Runner.Worker.dll")
	orig := append(append([]byte("MZ...."), utf16le("ACTIONS_RESULTS_URL")...), utf16le("ACTIONS_CACHE_URL")...)
	os.WriteFile(dll, orig, 0o644)
	if err := PatchWorker(dir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dll)
	if len(got) != len(orig) || !bytes.Contains(got, utf16le("ACTIONS_RESULTS_ORL")) || bytes.Contains(got, utf16le("ACTIONS_CACHE_URL")) {
		t.Fatalf("patch result wrong: %q", got)
	}
	if err := PatchWorker(dir); err != nil {
		t.Fatalf("patching twice must be a no-op: %v", err)
	}
	os.WriteFile(dll, []byte("unrelated"), 0o644)
	if err := PatchWorker(dir); err == nil {
		t.Fatal("an unknown runner layout must be reported")
	}
}

func TestSafeJoin(t *testing.T) {
	dir := t.TempDir()
	if _, err := safeJoin(dir, "../../etc/passwd"); err == nil {
		t.Fatal("path traversal must be rejected")
	}
	if _, err := safeJoin(dir, "bin/Runner.Listener"); err != nil {
		t.Fatal(err)
	}
}

func TestBannerAndCacheSummary(t *testing.T) {
	a, _ := New(&agentcfg.Config{Version: "v1", ImageName: "ubuntu24", Profile: "baked:standard", RunnerVersion: "2.338.2",
		Cache: agentcfg.CacheConfig{Enabled: true, Bucket: "b"}, KeepMinutes: 10,
		Job: agentcfg.JobInfo{Spec: "4 vCPU", Labels: []string{"truner/cpu=4"}, Warnings: []string{"w1"}}}, "")
	a.meta = Metadata{InstanceType: "c7a.xlarge", Lifecycle: "spot", AZ: "us-east-1a", AMI: "ami-1"}
	b := a.banner(true)
	for _, want := range []string{"truner | c7a.xlarge spot in us-east-1a", "::group::", "::endgroup::", "magic cache", "baked image ami-1", "::warning title=truner::w1", "stays up 10 min"} {
		if !strings.Contains(b, want) {
			t.Errorf("banner missing %q:\n%s", want, b)
		}
	}
	if s := cacheSummary(cacheproxy.Stats{}); !strings.Contains(s, "not used") {
		t.Error(s)
	}
	if s := cacheSummary(cacheproxy.Stats{Hits: 2, Misses: 1, BytesDown: 300 << 20, DownNanos: 1e9}); !strings.Contains(s, "2 hit(s)") || !strings.Contains(s, "300.0 MiB/s") {
		t.Error(s)
	}
}
