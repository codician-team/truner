package agent

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestHDDInstanceType(t *testing.T) {
	for typ, want := range map[string]bool{"d3en.xlarge": true, "d3.2xlarge": true, "d2.xlarge": true, "h1.4xlarge": true, "m7a.xlarge": false, "m6id.large": false, "r5d.8xlarge": false, "": false} {
		if got := hddInstanceType(typ); got != want {
			t.Errorf("%q: got %v", typ, got)
		}
	}
}

func TestPrefetch(t *testing.T) {
	dir := t.TempDir()
	for i, size := range []int{0, 10, 300 << 10} {
		sub := filepath.Join(dir, "bin", fmt.Sprint(i))
		os.MkdirAll(sub, 0o755)
		os.WriteFile(filepath.Join(sub, "f"), make([]byte, size), 0o644)
	}
	files, bytes, _ := prefetch([]string{filepath.Join(dir, "bin"), filepath.Join(dir, "missing")}, 4)
	if files != 3 || bytes != 10+300<<10 {
		t.Fatalf("prefetch read %d files, %d bytes", files, bytes)
	}
}

func TestNewestNode(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"node20", "node20_alpine", "node24", "node24_alpine", "nodejs"} {
		os.MkdirAll(filepath.Join(dir, d, "bin"), 0o755)
	}
	if got := newestNode(dir); got != filepath.Join(dir, "node24") {
		t.Fatalf("newestNode = %q", got)
	}
	if got := newestNode(filepath.Join(dir, "missing")); got != "" {
		t.Fatalf("no externals must give no node, got %q", got)
	}
}

func TestIdleLogDue(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	at := func(d time.Duration) time.Time { return start.Add(d) }
	if idleLogDue(start, time.Time{}, at(59*time.Second)) || !idleLogDue(start, time.Time{}, at(time.Minute)) {
		t.Fatal("the first upload is due after a minute")
	}
	if idleLogDue(start, at(time.Minute), at(5*time.Minute)) || !idleLogDue(start, at(time.Minute), at(6*time.Minute)) {
		t.Fatal("then every 5 minutes")
	}
}
