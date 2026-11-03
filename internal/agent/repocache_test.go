package agent

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/codician-team/truner/internal/agentcfg"
	"github.com/codician-team/truner/internal/cachestore"
)

func TestRepoScopes(t *testing.T) {
	for _, c := range []struct {
		ref, base, def string
		write          string
		read           []string
	}{
		{"refs/heads/main", "", "main", "refs/heads/main", []string{"refs/heads/main"}},
		{"refs/pull/7/merge", "main", "main", "refs/pull/7/merge", []string{"refs/pull/7/merge", "refs/heads/main"}},
		{"refs/pull/8/merge", "release", "main", "refs/pull/8/merge", []string{"refs/pull/8/merge", "refs/heads/release", "refs/heads/main"}},
		{"refs/tags/v1", "", "main", "refs/tags/v1", []string{"refs/tags/v1", "refs/heads/main"}},
		{"refs/heads/feature", "", "", "refs/heads/feature", []string{"refs/heads/feature"}},
	} {
		w, r := repoScopes(c.ref, c.base, c.def)
		if w != c.write || !slices.Equal(r, c.read) {
			t.Errorf("%s: got %s %v", c.ref, w, r)
		}
	}
}

func TestCheckoutURL(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"", "acme/app"}:                         "https://github.com/acme/app",
		{"https://github.com", "acme/app"}:       "https://github.com/acme/app",
		{"https://ghe.example.com/", "acme/app"}: "https://ghe.example.com/acme/app",
	} {
		if got := checkoutURL(in[0], in[1]); got != want {
			t.Errorf("%v: got %s", in, got)
		}
	}
}

// git runs git for a test, without the developer's configuration.
func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main", "-c", "safe.directory=*"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// objectCount counts a repository's objects, loose and packed.
func objectCount(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	for _, l := range strings.Split(testGit(t, dir, "count-objects", "-v"), "\n") {
		for _, k := range []string{"count: ", "in-pack: "} {
			if v, ok := strings.CutPrefix(l, k); ok {
				c, _ := strconv.Atoi(v)
				n += c
			}
		}
	}
	return n
}

// A job checks out the default branch and saves a snapshot. A pull request's
// checkout later restores it, and its shallow fetch transfers only what changed.
func TestRepoCacheRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	ctx := context.Background()
	tmp := t.TempDir()
	// As on a machine: the agent is root, checkouts belong to the runner user.
	asRunner := func(dir string) {}
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		tmp, _ = os.MkdirTemp("", "truner-repo-")
		t.Cleanup(func() { os.RemoveAll(tmp) })
		os.Chmod(tmp, 0o755)
		asRunner = func(dir string) {
			if out, err := exec.Command("chown", "-R", "65534:65534", dir).CombinedOutput(); err != nil {
				t.Fatalf("chown: %v %s", err, out)
			}
		}
	}

	// The repository on "GitHub": commit A with many files, then B changing one.
	src := filepath.Join(tmp, "src")
	os.MkdirAll(src, 0o755)
	testGit(t, src, "init", "-q")
	for i := range 40 {
		os.WriteFile(filepath.Join(src, "f"+strconv.Itoa(i)+".txt"), []byte(strings.Repeat("line "+strconv.Itoa(i)+"\n", 50)), 0o644)
	}
	testGit(t, src, "add", ".")
	testGit(t, src, "commit", "-q", "-m", "A")
	a := testGit(t, src, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(src, "f1.txt"), []byte("changed\n"), 0o644)
	testGit(t, src, "commit", "-q", "-am", "B")
	b := testGit(t, src, "rev-parse", "HEAD")
	testGit(t, src, "config", "uploadpack.allowAnySHA1InWant", "true")
	origin := "file://" + filepath.ToSlash(src)
	if runtime.GOOS == "windows" {
		origin = "file:///" + filepath.ToSlash(src)
	}

	event := filepath.Join(tmp, "event.json")
	os.WriteFile(event, []byte(`{"repository":{"default_branch":"main"}}`), 0o644)
	work := filepath.Join(tmp, "work")
	store := cachestore.NewMemory()
	git, _ := exec.LookPath("git")
	newCache := func() *repoCache {
		ag := &Agent{Cfg: &agentcfg.Config{Job: agentcfg.JobInfo{Repo: "acme/app"}}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		return &repoCache{a: ag, store: store, prefix: "cache/acme/app/", work: work, git: git, tried: map[string]bool{}}
	}
	// What actions/checkout does before its fetch, with the fetch sent to the
	// local repository.
	checkoutRepo := func(name string) string {
		dir := filepath.Join(work, name, name)
		os.MkdirAll(dir, 0o755)
		testGit(t, dir, "init", "-q")
		testGit(t, dir, "remote", "add", "origin", "https://github.com/acme/app")
		testGit(t, dir, "config", "url."+origin+".insteadOf", "https://github.com/acme/app")
		asRunner(filepath.Dir(dir))
		return dir
	}
	fetch := func(dir, commit string) {
		testGit(t, dir, "-c", "protocol.version=2", "fetch", "--no-tags", "--prune", "--no-recurse-submodules", "--depth=1", "origin", "+"+commit+":refs/remotes/origin/main")
		os.Remove(filepath.Join(dir, ".git", "refs", "truner", "cache")) // as the wrapper does
		testGit(t, dir, "checkout", "-q", "--force", "-B", "main", "refs/remotes/origin/main")
	}

	// A push to main: no snapshot yet; after the job, one is saved.
	c := newCache()
	c.setJob("acme/app", "https://github.com", "refs/heads/main", "", event)
	dir := checkoutRepo("app")
	if msg := c.restore(ctx, dir); !strings.Contains(msg, "no snapshot") {
		t.Fatalf("restore without a snapshot: %q", msg)
	}
	fetch(dir, a)
	if msg := c.save(ctx); !strings.Contains(msg, "saved a snapshot") {
		t.Fatalf("save: %q", msg)
	}
	key := cachestore.ObjectKey("cache/acme/app/", "refs/heads/main", repoCacheVersion, repoCacheKey)
	if _, err := store.Head(ctx, key); err != nil {
		t.Fatalf("snapshot not stored at %s: %v", key, err)
	}
	if msg := c.save(ctx); msg != "" {
		t.Fatalf("a job saves once: %q", msg)
	}

	// A pull request's checkout restores main's snapshot and fetches B.
	c = newCache()
	c.setJob("acme/app", "", "refs/pull/1/merge", "main", event)
	dir = checkoutRepo("pr")
	if msg := c.restore(ctx, dir); !strings.Contains(msg, "restored acme/app at "+a[:12]) || !strings.Contains(msg, "refs/heads/main") {
		t.Fatalf("restore: %q", msg)
	}
	restored := objectCount(t, dir)
	if restored < 42 {
		t.Fatalf("the snapshot must hold A's commit, tree and blobs: %d objects", restored)
	}
	fetch(dir, b)
	if got := objectCount(t, dir) - restored; got > 4 {
		t.Fatalf("the fetch must transfer only B's new objects, got %d", got)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "f1.txt")); string(data) != "changed\n" {
		t.Fatalf("checkout of B: f1.txt is %q", data)
	}
	if st := testGit(t, dir, "status", "--porcelain"); st != "" {
		t.Fatalf("clean checkout expected:\n%s", st)
	}
	testGit(t, dir, "fsck", "--connectivity-only", "--no-dangling")
	if msg := c.save(ctx); msg != "" {
		t.Fatalf("a pull request uses main's snapshot and saves none: %q", msg)
	}

	// Nothing for repositories that are not empty, of another repository, or
	// outside the work directory.
	c = newCache()
	c.setJob("acme/app", "", "refs/heads/main", "", event)
	if msg := c.restore(ctx, dir); msg != "" {
		t.Fatalf("a repository with objects: %q", msg)
	}
	other := checkoutRepo("other")
	testGit(t, other, "remote", "set-url", "origin", "https://github.com/acme/lib")
	if msg := c.restore(ctx, other); msg != "" {
		t.Fatalf("another repository: %q", msg)
	}
	outside := filepath.Join(tmp, "outside")
	os.MkdirAll(outside, 0o755)
	testGit(t, outside, "init", "-q")
	testGit(t, outside, "remote", "add", "origin", "https://github.com/acme/app")
	if msg := c.restore(ctx, outside); msg != "" {
		t.Fatalf("outside the work directory: %q", msg)
	}
	c.setJob("acme/app", "", "refs/heads/evil", "", event)
	if c.job.write != "refs/heads/main" {
		t.Fatalf("the job-started hook's scopes must not change: %+v", c.job)
	}
}

func TestRepoCacheLocalOnly(t *testing.T) {
	c := &repoCache{}
	h := c.local(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	for addr, want := range map[string]int{"127.0.0.1:5000": http.StatusTeapot, "[::1]:5000": http.StatusTeapot, "10.0.3.7:5000": http.StatusForbidden} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/_truner/repo/restore", nil)
		r.RemoteAddr = addr
		h(rec, r)
		if rec.Code != want {
			t.Errorf("%s: got %d", addr, rec.Code)
		}
	}
}

// The wrapper asks for a restore before a shallow fetch into an empty
// repository, deletes the cache ref afterwards and keeps git's exit code.
// Everything else goes straight to git.
func TestGitWrapper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the wrapper is a POSIX shell script")
	}
	for _, tool := range []string{"sh", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
	tmp := t.TempDir()
	calls := filepath.Join(tmp, "calls")
	fake := filepath.Join(tmp, "realgit")
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"$*\" >>"+calls+"\n[ \"$1\" = update-ref ] && exit 0\nexit 3\n"), 0o755)
	var asked []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path+" "+r.FormValue("dir"))
		os.MkdirAll(filepath.Join(r.FormValue("dir"), ".git", "refs", "truner"), 0o755)
		os.WriteFile(filepath.Join(r.FormValue("dir"), ".git", "refs", "truner", "cache"), []byte("x\n"), 0o644)
		io.WriteString(w, "restored\n")
	}))
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	wrapper := filepath.Join(tmp, "git")
	os.WriteFile(wrapper, []byte(gitWrapper(fake, ln.Addr().(*net.TCPAddr).Port)), 0o755)
	repo := filepath.Join(tmp, "repo")
	os.MkdirAll(filepath.Join(repo, ".git", "objects", "pack"), 0o755)
	run := func(args ...string) (int, string) {
		cmd := exec.Command(wrapper, args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return code, string(out)
	}

	code, out := run("-c", "protocol.version=2", "fetch", "--no-tags", "--depth=1", "origin", "+abc:refs/remotes/origin/main")
	if code != 3 || !strings.Contains(out, "restored") {
		t.Fatalf("fetch: exit %d, output %q", code, out)
	}
	if len(asked) != 1 || asked[0] != "/_truner/repo/restore "+repo {
		t.Fatalf("restore requests: %v", asked)
	}
	got, _ := os.ReadFile(calls)
	want := "-c protocol.version=2 fetch --no-tags --depth=1 origin +abc:refs/remotes/origin/main\nupdate-ref -d refs/truner/cache\n"
	if string(got) != want {
		t.Fatalf("git calls:\n%s\nwant:\n%s", got, want)
	}

	// A repository with objects, a full fetch and other commands: only git.
	os.Remove(calls)
	os.WriteFile(filepath.Join(repo, ".git", "objects", "pack", "pack-1.pack"), nil, 0o644)
	run("fetch", "--depth=1", "origin", "main")
	os.Remove(filepath.Join(repo, ".git", "objects", "pack", "pack-1.pack"))
	run("fetch", "origin", "main")
	if code, _ := run("status", "--short"); code != 3 {
		t.Fatalf("exit code must be git's, got %d", code)
	}
	got, _ = os.ReadFile(calls)
	if len(asked) != 1 || string(got) != "fetch --depth=1 origin main\nfetch origin main\nstatus --short\n" {
		t.Fatalf("requests %v, git calls:\n%s", asked, got)
	}
}
