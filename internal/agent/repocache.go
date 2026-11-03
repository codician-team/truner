package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/codician-team/truner/internal/cachestore"
)

// The repository cache keeps a snapshot of a repository's checked-out commit,
// its objects in one git pack, in the magic cache's S3 prefix. Snapshots are
// scoped like cache entries: a job reads the one of its ref, its base branch
// or the default branch, and writes to its own ref.
//
// A git wrapper first on the runner's PATH asks the agent to restore the
// snapshot when a shallow fetch starts in an empty repository of the job's
// repository, which is what actions/checkout does. The repository then has
// the snapshot's commit (as a shallow commit), and the fetch transfers only
// the objects that are newer. After the job, the agent saves a new snapshot
// when there was none for the job's ref or it is old.

const (
	repoCacheVersion = "truner-repo-1"
	repoCacheKey     = "truner-repository-snapshot"
	// repoSnapshotMaxAge is when the default branch's snapshot is replaced.
	repoSnapshotMaxAge  = 6 * time.Hour
	repoSnapshotMaxSize = 4 << 30
	// repoCacheRef makes the snapshot's commit known to the fetch; the
	// wrapper deletes it after the fetch.
	repoCacheRef = "refs/truner/cache"
)

var (
	packFileRe = regexp.MustCompile(`^pack-[0-9a-f]{40}\.(pack|idx)$`)
	commitRe   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type repoCache struct {
	a      *Agent
	store  cachestore.Store
	prefix string
	work   string // the runner's work directory: checkouts are below it
	git    string // the git the wrapper runs

	mu    sync.Mutex
	job   *repoJob
	tried map[string]bool // checkouts the wrapper asked about
	dir   string          // the job's checkout, saved after the job
	found string          // scope of the restored snapshot
	age   time.Duration   // of the restored snapshot
	saved bool
}

// repoJob is what the job-started hook reports about the job.
type repoJob struct {
	url           string // remote URL as actions/checkout sets it
	write         string
	read          []string
	defaultBranch string
}

// repoScopes mirrors the cache's branch isolation: write to the job's ref,
// read it, then the base branch of a pull request, then the default branch.
func repoScopes(ref, base, defaultBranch string) (write string, read []string) {
	add := func(s string) {
		for _, r := range read {
			if r == s {
				return
			}
		}
		read = append(read, s)
	}
	if ref != "" {
		add(ref)
	}
	if base != "" {
		add("refs/heads/" + base)
	}
	if defaultBranch != "" {
		add("refs/heads/" + defaultBranch)
	}
	return ref, read
}

// checkoutURL is the remote URL actions/checkout gives a repository.
func checkoutURL(server, repo string) string {
	u, err := url.Parse(server)
	if server == "" || err != nil || u.Scheme == "" || u.Host == "" {
		u, _ = url.Parse("https://github.com")
	}
	owner, name, _ := strings.Cut(repo, "/")
	return u.Scheme + "://" + u.Host + "/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func (c *repoCache) handle(mux *http.ServeMux) {
	mux.HandleFunc("POST /_truner/repo/job", c.local(func(w http.ResponseWriter, r *http.Request) {
		c.setJob(r.FormValue("repository"), r.FormValue("server"), r.FormValue("ref"), r.FormValue("base"), r.FormValue("event"))
	}))
	mux.HandleFunc("POST /_truner/repo/restore", c.local(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
		defer cancel()
		fmt.Fprint(w, line(c.restore(ctx, r.FormValue("dir"))))
	}))
	mux.HandleFunc("POST /_truner/repo/save", c.local(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		fmt.Fprint(w, line(c.save(ctx)))
	}))
}

func line(s string) string {
	if s == "" {
		return ""
	}
	return s + "\n"
}

// local refuses requests from other machines: the cache port is open to the
// job's containers.
func (c *repoCache) local(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// setJob records the job's repository and scopes. The job-started hook calls
// it before any step runs; later calls change nothing.
func (c *repoCache) setJob(repo, server, ref, base, eventPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.job != nil || repo == "" || !strings.EqualFold(repo, c.a.Cfg.Job.Repo) {
		return
	}
	var ev struct {
		Repository struct {
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
	}
	if data, err := os.ReadFile(eventPath); err == nil {
		json.Unmarshal(data, &ev)
	}
	write, read := repoScopes(ref, base, ev.Repository.DefaultBranch)
	c.job = &repoJob{url: checkoutURL(server, repo), write: write, read: read, defaultBranch: ev.Repository.DefaultBranch}
}

// restore puts the newest snapshot into the empty repository at dir. It
// returns a line for the job log, or "" when it does nothing.
func (c *repoCache) restore(ctx context.Context, dir string) string {
	dir = filepath.Clean(dir)
	c.mu.Lock()
	job := c.job
	if job == nil || c.tried[dir] {
		c.mu.Unlock()
		return ""
	}
	c.tried[dir] = true
	c.mu.Unlock()
	gitDir := filepath.Join(dir, ".git")
	if !strings.HasPrefix(dir, c.work+string(filepath.Separator)) || !emptyRepo(gitDir) || !strings.EqualFold(c.config(gitDir, "remote.origin.url"), job.url) {
		return ""
	}
	if f := c.config(gitDir, "extensions.objectformat"); f != "" && f != "sha1" {
		return ""
	}
	c.mu.Lock()
	if c.dir == "" {
		c.dir = dir
	}
	c.mu.Unlock()

	start := time.Now()
	var obj *cachestore.Object
	var scope string
	for _, s := range job.read {
		o, err := c.store.Head(ctx, cachestore.ObjectKey(c.prefix, s, repoCacheVersion, repoCacheKey))
		if err == nil {
			obj, scope = o, s
			break
		}
		if !errors.Is(err, cachestore.ErrNotFound) {
			c.a.Log.Warn("repository snapshot lookup failed", "scope", s, "err", err)
		}
	}
	if obj == nil {
		return "truner: no snapshot of this repository in S3 yet, fetching it from GitHub"
	}
	c.mu.Lock()
	c.found, c.age = scope, time.Since(obj.LastModified)
	c.mu.Unlock()
	commit, err := c.unpack(ctx, obj.Key, gitDir)
	if err != nil {
		os.Remove(filepath.Join(gitDir, "shallow"))
		os.Remove(filepath.Join(gitDir, filepath.FromSlash(repoCacheRef)))
		c.a.Log.Warn("repository snapshot restore failed", "key", obj.Key, "err", err)
		return "truner: could not restore the repository snapshot from S3, fetching everything from GitHub: " + err.Error()
	}
	took := time.Since(start)
	c.a.Log.Info("repository snapshot restored", "scope", scope, "commit", commit, "bytes", obj.Size, "took", took.Round(time.Millisecond))
	return fmt.Sprintf("truner: restored %s at %s from S3 (%s, %s, %s old, in %.1fs); fetching only newer objects from GitHub",
		c.a.Cfg.Job.Repo, commit[:12], scope, humanBytes(obj.Size), age(time.Since(obj.LastModified)), took.Seconds())
}

// emptyRepo reports whether gitDir is a repository without objects or refs:
// one that `git init` just created.
func emptyRepo(gitDir string) bool {
	if fi, err := os.Stat(gitDir); err != nil || !fi.IsDir() {
		return false
	}
	if _, err := os.Stat(filepath.Join(gitDir, "shallow")); err == nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(gitDir, "packed-refs")); err == nil {
		return false
	}
	entries, err := os.ReadDir(filepath.Join(gitDir, "objects"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		switch e.Name() {
		case "info":
		case "pack":
			if packs, _ := os.ReadDir(filepath.Join(gitDir, "objects", "pack")); len(packs) > 0 {
				return false
			}
		default:
			return false // loose objects
		}
	}
	return true
}

// unpack writes the snapshot's pack into gitDir and makes its commit a known,
// shallow commit. The pack becomes visible only once complete.
func (c *repoCache) unpack(ctx context.Context, key, gitDir string) (string, error) {
	body, err := c.store.GetRange(ctx, key, 0, -1)
	if err != nil {
		return "", err
	}
	defer body.Close()
	packDir := filepath.Join(gitDir, "objects", "pack")
	tmp := map[string]string{} // final name -> temporary file
	defer func() {
		for _, t := range tmp {
			os.Remove(t)
		}
	}()
	var commit string
	tr := tar.NewReader(body)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		switch {
		case h.Name == "commit":
			b, _ := io.ReadAll(io.LimitReader(tr, 100))
			commit = strings.TrimSpace(string(b))
		case packFileRe.MatchString(h.Name) && h.Typeflag == tar.TypeReg:
			f, err := os.CreateTemp(packDir, "tmp_truner_")
			if err != nil {
				return "", err
			}
			tmp[h.Name] = f.Name()
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return "", err
			}
		}
	}
	var pack, idx string
	for name := range tmp {
		if strings.HasSuffix(name, ".pack") {
			pack = name
			idx = strings.TrimSuffix(name, ".pack") + ".idx"
		}
	}
	if !commitRe.MatchString(commit) || pack == "" || tmp[idx] == "" || len(tmp) != 2 {
		return "", errors.New("the snapshot is incomplete")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	uid, gid, owned := osOwner(gitDir)
	install := func(path string, data []byte) error {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
		if owned {
			os.Chown(filepath.Dir(path), uid, gid)
			os.Chown(path, uid, gid)
		}
		return nil
	}
	// The pack first, its index last: git only sees packs with an index.
	for _, name := range []string{pack, idx} {
		if owned {
			os.Chown(tmp[name], uid, gid)
		}
		os.Chmod(tmp[name], 0o444)
		if err := os.Rename(tmp[name], filepath.Join(packDir, name)); err != nil {
			return "", err
		}
		delete(tmp, name)
	}
	if err := install(filepath.Join(gitDir, "shallow"), []byte(commit+"\n")); err != nil {
		return "", err
	}
	return commit, install(filepath.Join(gitDir, filepath.FromSlash(repoCacheRef)), []byte(commit+"\n"))
}

// save stores a snapshot of the job's checkout after the job, when the job's
// ref has none that is recent. A pull request or another branch saves only
// when no snapshot was found at all: the default branch's serves it.
func (c *repoCache) save(ctx context.Context) string {
	c.mu.Lock()
	job, dir, found, old, saved := c.job, c.dir, c.found, c.age, c.saved
	c.saved = true
	c.mu.Unlock()
	if saved || job == nil || dir == "" || job.write == "" {
		return ""
	}
	switch {
	case found == "":
	case found == job.write && old > repoSnapshotMaxAge:
	case found != job.write && job.write == "refs/heads/"+job.defaultBranch:
	default:
		return ""
	}
	gitDir := filepath.Join(dir, ".git")
	if c.config(gitDir, "extensions.partialclone") != "" || c.config(gitDir, "remote.origin.promisor") == "true" {
		return "" // objects are missing by design
	}
	start := time.Now()
	n, commit, err := c.snapshot(ctx, dir, cachestore.ObjectKey(c.prefix, job.write, repoCacheVersion, repoCacheKey))
	if err != nil {
		c.a.Log.Warn("repository snapshot not saved", "err", err)
		return "truner: could not save a repository snapshot to S3: " + err.Error()
	}
	c.a.Log.Info("repository snapshot saved", "scope", job.write, "commit", commit, "bytes", n, "took", time.Since(start).Round(time.Millisecond))
	return fmt.Sprintf("truner: saved a snapshot of %s at %s to S3 (%s, %s) for later checkouts", c.a.Cfg.Job.Repo, commit[:12], job.write, humanBytes(n))
}

// snapshot packs the objects of HEAD's commit and tree, without history, and
// uploads them with the commit as a tar file.
func (c *repoCache) snapshot(ctx context.Context, dir, key string) (int64, string, error) {
	out, err := c.gitOut(ctx, dir, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return 0, "", err
	}
	commit := strings.TrimSpace(string(out))
	if !commitRe.MatchString(commit) {
		return 0, "", fmt.Errorf("unexpected commit %q", commit)
	}
	objects, err := c.gitOut(ctx, dir, nil, "rev-list", "--objects", "--no-walk", commit)
	if err != nil {
		return 0, "", err
	}
	tmp, err := os.MkdirTemp(filepath.Join(dir, ".git"), "truner-snapshot-")
	if err != nil {
		return 0, "", err
	}
	defer os.RemoveAll(tmp)
	if uid, gid, ok := osOwner(filepath.Join(dir, ".git")); ok {
		os.Chown(tmp, uid, gid)
	}
	out, err = c.gitOut(ctx, dir, objects, "pack-objects", "--delta-base-offset", "-q", filepath.Join(tmp, "pack"))
	if err != nil {
		return 0, "", err
	}
	name := "pack-" + strings.TrimSpace(string(out))
	files := []string{name + ".pack", name + ".idx"}
	var size int64
	for _, f := range files {
		fi, err := os.Stat(filepath.Join(tmp, f))
		if err != nil {
			return 0, "", err
		}
		size += fi.Size()
	}
	if size > repoSnapshotMaxSize {
		return 0, "", fmt.Errorf("the snapshot is %s, more than %s", humanBytes(size), humanBytes(repoSnapshotMaxSize))
	}
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeSnapshot(pw, commit, tmp, files))
	}()
	if err := c.store.Put(ctx, key, pr, -1); err != nil {
		pr.CloseWithError(err)
		return 0, "", err
	}
	return size, commit, nil
}

func writeSnapshot(w io.Writer, commit, dir string, files []string) error {
	tw := tar.NewWriter(w)
	c := []byte(commit + "\n")
	if err := tw.WriteHeader(&tar.Header{Name: "commit", Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(c); err != nil {
		return err
	}
	for _, name := range files {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		fi, _ := f.Stat()
		err = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o444, Size: fi.Size(), Typeflag: tar.TypeReg})
		if err == nil {
			_, err = io.Copy(tw, f)
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

// config reads one value of a repository's config file.
func (c *repoCache) config(gitDir, key string) string {
	cmd := exec.Command(c.git, "config", "--file", filepath.Join(gitDir, "config"), "--get", key)
	cmd.Dir = os.TempDir() // outside any repository
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// gitOut runs git in dir as the owner of the repository.
func (c *repoCache) gitOut(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.git, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	osRunAsOwner(cmd, filepath.Join(dir, ".git"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(tail(stderr.String(), 300)))
	}
	return out, nil
}

func age(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// gitWrapper is the git the runner finds first on its PATH. It runs the real
// git, and before a shallow fetch into an empty repository asks the agent to
// restore the repository's snapshot, then deletes the ref the fetch needed.
func gitWrapper(realGit string, port int) string {
	return fmt.Sprintf(`#!/bin/sh
# truner: restores the repository snapshot before actions/checkout's fetch,
# then runs git.
case " $* " in
*" fetch "*" --depth"*)
	if [ -d .git/objects/pack ] && [ -z "$(ls -A .git/objects/pack)" ]; then
		curl -fsS -m 300 --data-urlencode "dir=$PWD" http://127.0.0.1:%[2]d/_truner/repo/restore >&2 2>/dev/null
		"%[1]s" "$@"
		rc=$?
		if [ -f .git/%[3]s ]; then "%[1]s" update-ref -d %[3]s >/dev/null 2>&1; fi
		exit $rc
	fi
	;;
esac
exec "%[1]s" "$@"
`, realGit, port, repoCacheRef)
}

// lookPathIn finds an executable in a PATH list.
func lookPathIn(name, path string) string {
	for _, d := range filepath.SplitList(path) {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}
