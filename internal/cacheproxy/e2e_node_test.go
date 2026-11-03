package cacheproxy

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/truner/truner/internal/cachestore"
)

// TestNodeToolkitE2E runs the real @actions/cache toolkit against the proxy.
//
//	TRUNER_NODE_E2E=/path/to/dir-with-node_modules go test ./internal/cacheproxy -run NodeToolkit -v
//
// The directory must contain node_modules/@actions/cache and the e2e.mjs
// script from testdata/.
func TestNodeToolkitE2E(t *testing.T) {
	dir := os.Getenv("TRUNER_NODE_E2E")
	if dir == "" {
		t.Skip("set TRUNER_NODE_E2E to run against the real @actions/cache toolkit")
	}
	script, err := os.ReadFile(filepath.Join("testdata", "e2e.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "e2e.mjs"), script, 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	s, err := New(Options{Store: cachestore.Dir{Root: t.TempDir()}, Prefix: "cache/acme/app/", PublicURL: base, TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(ln, s)

	payload, _ := json.Marshal(map[string]string{"ac": `[{"Scope":"refs/heads/main","Permission":3}]`})
	work := t.TempDir()
	cmd := exec.Command("node", "e2e.mjs", work)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"ACTIONS_RESULTS_URL="+base+"/",
		"ACTIONS_CACHE_URL="+base+"/",
		"ACTIONS_CACHE_SERVICE_V2=True",
		"ACTIONS_RUNTIME_TOKEN=x."+base64.RawURLEncoding.EncodeToString(payload)+".sig",
		"RUNNER_TEMP="+filepath.Join(work, "tmp"),
		"GITHUB_SERVER_URL=https://github.com",
	)
	out, err := cmd.CombinedOutput()
	t.Logf("node output:\n%s", out)
	if err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	t.Logf("stats: %+v", st)
	if st.Hits < 2 || st.Saves < 2 {
		t.Fatalf("expected hits and saves, got %+v", st)
	}
}
