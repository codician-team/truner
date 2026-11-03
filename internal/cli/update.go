package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/codician-team/truner/internal/version"
)

// Variables for tests: where GitHub serves the releases, the path of this CLI
// and how it runs again after an update.
var (
	releasesBase = "https://github.com"
	executable   = os.Executable
	runUpdated   = rerun
)

// updatedEnv tells the CLI that it was just updated and started again, so it
// does not update itself a second time.
const updatedEnv = "TRUNER_CLI_UPDATED_FROM"

// latestRelease returns the tag of the newest truner release, or "" when it
// cannot be found out within a few seconds. GitHub redirects
// /releases/latest to the release's tag, without the API's rate limit.
func latestRelease(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, releasesBase+"/"+version.Repo+"/releases/latest", nil)
	if err != nil {
		return ""
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return ""
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	tag := path.Base(loc)
	if _, ok := version.Compare(tag, tag); !ok || !strings.Contains(loc, "/releases/tag/") {
		return ""
	}
	return tag
}

// newerRelease returns the newest release when it is newer than this CLI.
func newerRelease(ctx context.Context) string {
	if version.IsDev() {
		return ""
	}
	latest := latestRelease(ctx)
	if c, ok := version.Compare(version.Version, latest); ok && c < 0 {
		return latest
	}
	return ""
}

// change says what deploying version to over an installation of version from
// does: "upgraded from v0.1.3 to v0.1.4", "redeployed with v0.1.4", ...
func change(from, to string) string {
	if from == to {
		return "redeployed with " + to
	}
	if from == "" {
		from = "an unknown version"
	}
	switch c, ok := version.Compare(to, from); {
	case ok && c > 0:
		return fmt.Sprintf("upgraded from %s to %s", from, to)
	case ok && c < 0:
		return fmt.Sprintf("downgraded from %s to %s", from, to)
	}
	return fmt.Sprintf("updated from %s to %s", from, to)
}

// releaseBinary is the name of this platform's CLI in a release.
func releaseBinary() string {
	name := fmt.Sprintf("truner_%s_%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// selfUpdate replaces this CLI with the release tag, checked against the
// release's checksums, and returns the path of the CLI.
func selfUpdate(ctx context.Context, u *ui, tag string) (string, error) {
	exe, err := executable()
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	base := releasesBase + "/" + version.Repo + "/releases/download/" + tag + "/"
	name := releaseBinary()
	sums, err := download(ctx, base+"checksums.txt", 1<<20)
	if err != nil {
		return exe, err
	}
	want := ""
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == name {
			want = f[0]
		}
	}
	if want == "" {
		return exe, fmt.Errorf("release %s has no %s", tag, name)
	}
	sp := u.spin(fmt.Sprintf("Downloading truner %s", tag))
	data, err := download(ctx, base+name, 1<<30)
	sp.done()
	if err != nil {
		return exe, err
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != want {
		return exe, fmt.Errorf("checksum mismatch for %s", name)
	}
	// Next to the CLI, so that the rename stays on one file system.
	tmp := filepath.Join(filepath.Dir(exe), ".truner-"+tag+".new")
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return exe, err
	}
	if runtime.GOOS == "windows" {
		// A running program cannot be overwritten, but it can be renamed.
		old := exe + ".old"
		os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			os.Remove(tmp)
			return exe, err
		}
		if err := os.Rename(tmp, exe); err != nil {
			os.Rename(old, exe)
			return exe, err
		}
		return exe, nil
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return exe, err
	}
	return exe, nil
}

// removeOldBinary deletes what a Windows self-update left behind.
func removeOldBinary() {
	if runtime.GOOS != "windows" {
		return
	}
	if exe, err := executable(); err == nil {
		os.Remove(exe + ".old")
	}
}

func download(ctx context.Context, url string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// exitCode ends the CLI with a code and no message of its own: the command
// that ran in its place printed it.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// rerun runs the same command with the updated CLI at exe.
func rerun(exe string) error {
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), updatedEnv+"="+version.Version)
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return exitCode(ee.ExitCode())
	}
	return err
}

// releaseLinks says where to read what changed from one release to a newer one.
func releaseLinks(u *ui, from, to string) {
	if c, ok := version.Compare(to, from); !ok || c <= 0 {
		return
	}
	repo := "https://github.com/" + version.Repo
	u.printf("\nWhat's new in truner %s:\n\n    %s\n", to, u.cyan(repo+"/releases/tag/"+to))
	u.printf("\nReport any bugs:\n\n    %s\n", u.cyan(repo+"/issues"))
	u.printf("\nCommit log:\n\n    %s\n", u.cyan(repo+"/compare/"+from+"..."+to))
}
