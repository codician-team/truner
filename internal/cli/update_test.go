package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codician-team/truner/internal/version"
)

func TestChange(t *testing.T) {
	for _, c := range []struct{ from, to, want string }{
		{"v0.1.3", "v0.1.4", "upgraded from v0.1.3 to v0.1.4"},
		{"v0.1.3", "v0.1.1", "downgraded from v0.1.3 to v0.1.1"},
		{"v0.1.4", "v0.1.4", "redeployed with v0.1.4"},
		{"dev-1a2b", "v0.1.4", "updated from dev-1a2b to v0.1.4"},
		{"", "v0.1.4", "updated from an unknown version to v0.1.4"},
	} {
		if got := change(c.from, c.to); got != c.want {
			t.Errorf("%s -> %s: got %q", c.from, c.to, got)
		}
	}
}

// fakeReleases serves GitHub's redirect from /releases/latest to the tag of
// latest ("" for none), and that release's CLI and checksums.
type fakeReleases struct {
	latest string
	binary []byte
	sum    string // checksum to publish, the binary's by default
}

func (f *fakeReleases) serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/" + version.Repo + "/releases/"
		switch {
		case r.URL.Path == prefix+"latest" && f.latest != "":
			http.Redirect(w, r, "https://github.com"+prefix+"tag/"+f.latest, http.StatusFound)
		case r.URL.Path == prefix+"download/"+f.latest+"/checksums.txt":
			sum := f.sum
			if sum == "" {
				s := sha256.Sum256(f.binary)
				sum = hex.EncodeToString(s[:])
			}
			w.Write([]byte("0000 truner_other_os\n" + sum + "  " + releaseBinary() + "\n"))
		case r.URL.Path == prefix+"download/"+f.latest+"/"+releaseBinary():
			w.Write(f.binary)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := releasesBase
	releasesBase = srv.URL
	t.Cleanup(func() { releasesBase = old })
}

func TestNewerRelease(t *testing.T) {
	defer func(v string) { version.Version = v }(version.Version)
	ctx := context.Background()
	(&fakeReleases{latest: "v0.1.4"}).serve(t)
	for v, want := range map[string]string{"v0.1.1": "v0.1.4", "v0.1.4": "", "v0.1.5": "", "v0.1.5-rc.1": "", "dev": ""} {
		version.Version = v
		if got := newerRelease(ctx); got != want {
			t.Errorf("CLI %s: got %q, want %q", v, got, want)
		}
	}
	version.Version = "v0.1.1"
	(&fakeReleases{}).serve(t)
	if got := newerRelease(ctx); got != "" {
		t.Errorf("no redirect: %q", got)
	}
}

// An outdated CLI updates itself to the latest release and runs the command
// again with it, before it touches AWS.
func TestUpgradeUpdatesTheCLIFirst(t *testing.T) {
	defer func(v string) { version.Version = v }(version.Version)
	version.Version = "v0.1.1"
	isolateAWS(t)
	t.Setenv(updatedEnv, "")
	exe := filepath.Join(t.TempDir(), "truner")
	var reran []string
	oldExe, oldRun := executable, runUpdated
	executable = func() (string, error) { return exe, nil }
	runUpdated = func(p string) error { reran = append(reran, p); return exitCode(0) }
	t.Cleanup(func() { executable, runUpdated = oldExe, oldRun })
	run := func(f *installFlags) (string, error) {
		os.WriteFile(exe, []byte("old cli"), 0o755)
		reran = nil
		var out bytes.Buffer
		a := &app{ui: &ui{out: &out, in: bufio.NewReader(strings.NewReader(""))}, stack: "truner"}
		err := a.install(context.Background(), f, a.upgradeCmd())
		return out.String(), err
	}

	rel := &fakeReleases{latest: "v0.1.4", binary: []byte("new cli")}
	rel.serve(t)
	out, err := run(&installFlags{upgradeOnly: true})
	var code exitCode
	if !errors.As(err, &code) || code != 0 || len(reran) != 1 || reran[0] != exe {
		t.Fatalf("the updated CLI must run the command again: %v %v\n%s", err, reran, out)
	}
	if data, _ := os.ReadFile(exe); string(data) != "new cli" {
		t.Fatalf("the CLI was not replaced: %q", data)
	}
	if !strings.Contains(out, "truner v0.1.4 is out!") || !strings.Contains(out, "You're on v0.1.1.") || !strings.Contains(out, "Updated the CLI at "+exe+" to v0.1.4") || strings.Contains(out, "[1/5]") {
		t.Fatalf("output:\n%s", out)
	}
	removeOldBinary() // what Windows leaves behind is gone on the next start
	if _, err := os.Stat(exe + ".old"); err == nil {
		t.Fatal("the old binary must be removed")
	}

	// A bad download changes nothing and says how to update by hand.
	rel.sum = strings.Repeat("0", 64)
	out, err = run(&installFlags{upgradeOnly: true})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") || !strings.Contains(err.Error(), "--no-cli-update") || !strings.Contains(out, "ACTION REQUIRED") || len(reran) != 0 {
		t.Fatalf("bad checksum: %v\n%s", err, out)
	}
	if data, _ := os.ReadFile(exe); string(data) != "old cli" {
		t.Fatalf("the CLI must be left alone: %q", data)
	}
	rel.sum = ""

	// --no-cli-update, or a CLI that was just updated, goes on to AWS (which
	// has no credentials here) with its own version.
	out, err = run(&installFlags{upgradeOnly: true, noCLIUpdate: true})
	if len(reran) != 0 || !strings.Contains(out, "--no-cli-update") || !strings.Contains(out, "[1/5] AWS account") {
		t.Fatalf("--no-cli-update: %v\n%s", err, out)
	}
	t.Setenv(updatedEnv, "v0.1.0")
	if out, _ := run(&installFlags{upgradeOnly: true}); len(reran) != 0 || !strings.Contains(out, "[1/5] AWS account") {
		t.Fatalf("an updated CLI must not update again:\n%s", out)
	}
}

func TestUpgradeCLIOnly(t *testing.T) {
	defer func(v string) { version.Version = v }(version.Version)
	version.Version = "v0.1.4"
	(&fakeReleases{latest: "v0.1.4"}).serve(t)
	run := func() string {
		var out bytes.Buffer
		a := &app{ui: &ui{out: &out}, stack: "truner"}
		if err := a.install(context.Background(), &installFlags{upgradeOnly: true, cliOnly: true}, a.upgradeCmd()); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	t.Setenv(updatedEnv, "")
	if out := run(); !strings.Contains(out, "The CLI is up to date (v0.1.4)") || strings.Contains(out, "[1/5]") {
		t.Fatalf("up to date:\n%s", out)
	}
	t.Setenv(updatedEnv, "v0.1.2")
	out := run()
	for _, want := range []string{"Welcome to truner v0.1.4!", "releases/tag/v0.1.4", "/issues", "compare/v0.1.2...v0.1.4"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q after an update:\n%s", want, out)
		}
	}
}
