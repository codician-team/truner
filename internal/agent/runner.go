package agent

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf16"
)

const versionFile = ".truner-runner-version"

// ensureRunner makes sure the requested actions/runner version is unpacked in dir.
func (a *Agent) ensureRunner(ctx context.Context, dir string) error {
	want := a.Cfg.RunnerVersion
	have, _ := os.ReadFile(filepath.Join(dir, versionFile))
	if want == "" {
		if len(have) > 0 {
			a.Cfg.RunnerVersion = strings.TrimSpace(string(have))
			return nil
		}
		return errors.New("no runner version known and none preinstalled")
	}
	if strings.TrimSpace(string(have)) == want {
		a.Log.Info("runner already installed", "version", want)
		return nil
	}
	if len(a.Cfg.RunnerURLs) == 0 {
		return errors.New("no runner download URLs")
	}
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		for _, u := range a.Cfg.RunnerURLs {
			start := time.Now()
			err := downloadAndExtract(ctx, u, dir)
			if err == nil {
				a.Log.Info("runner downloaded", "version", want, "from", redact(u), "took", time.Since(start).Round(time.Millisecond))
				return os.WriteFile(filepath.Join(dir, versionFile), []byte(want), 0o644)
			}
			lastErr = err
			a.Log.Warn("runner download failed", "from", redact(u), "err", err)
		}
		time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
	}
	return fmt.Errorf("download runner: %w", lastErr)
}

func redact(u string) string {
	if i := strings.Index(u, "?"); i > 0 {
		return u[:i]
	}
	return u
}

func downloadAndExtract(ctx context.Context, url, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET: %s", resp.Status)
	}
	if strings.Contains(strings.SplitN(url, "?", 2)[0], ".zip") {
		return extractZip(resp.Body, dir)
	}
	return extractTarGz(bufio.NewReaderSize(resp.Body, 1<<20), dir)
}

func safeJoin(dir, name string) (string, error) {
	p := filepath.Join(dir, filepath.FromSlash(name))
	if p != dir && !strings.HasPrefix(p, dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe path in archive: %s", name)
	}
	return p, nil
}

func extractTarGz(r io.Reader, dir string) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		p, err := safeJoin(dir, h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(p), 0o755)
			f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode)&0o777|0o200)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.Remove(p)
			if err := os.Symlink(h.Linkname, p); err != nil {
				return err
			}
		}
	}
}

func extractZip(r io.Reader, dir string) error {
	tmp, err := os.CreateTemp("", "runner-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, err := io.Copy(tmp, r)
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(tmp, n)
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		p, err := safeJoin(dir, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(p, 0o755)
			continue
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(p)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// patchPairs renames the env vars the runner force-sets for actions, so the
// values truner puts in the runner environment (pointing at the local cache
// proxy) are the ones actions see. Replacements keep the exact length.
var patchPairs = [][2]string{
	{"ACTIONS_RESULTS_URL", "ACTIONS_RESULTS_ORL"},
	{"ACTIONS_CACHE_URL", "ACTIONS_CACHE_ORL"},
}

func utf16le(s string) []byte {
	var b bytes.Buffer
	for _, r := range utf16.Encode([]rune(s)) {
		b.WriteByte(byte(r))
		b.WriteByte(byte(r >> 8))
	}
	return b.Bytes()
}

// PatchWorker patches bin/Runner.Worker.dll in a runner directory. It is
// idempotent and fails (leaving the file untouched) if the runner layout changed.
func PatchWorker(runnerDir string) error {
	p := filepath.Join(runnerDir, "bin", "Runner.Worker.dll")
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	out := data
	for _, pair := range patchPairs {
		from, to := utf16le(pair[0]), utf16le(pair[1])
		switch {
		case bytes.Contains(out, from):
			out = bytes.ReplaceAll(out, from, to)
		case bytes.Contains(out, to):
			// already patched
		default:
			return fmt.Errorf("runner layout changed: %s not found in Runner.Worker.dll", pair[0])
		}
	}
	if bytes.Equal(out, data) {
		return nil
	}
	tmp := p + ".truner"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// runnerProcess runs the runner and watches it.
type runnerProcess struct {
	cmd       *exec.Cmd
	jobSeen   atomic.Bool
	jobDone   atomic.Bool
	exitErr   error
	done      chan struct{}
	stateFile string
}

func (a *Agent) startRunner(cmd *exec.Cmd) (*runnerProcess, error) {
	rp := &runnerProcess{cmd: cmd, done: make(chan struct{}), stateFile: filepath.Join(stateDir(), "job-started")}
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			a.Log.Info("runner: " + line)
			switch {
			case strings.Contains(line, "Running job:"):
				rp.jobSeen.Store(true)
			case strings.Contains(line, "completed with result:"):
				rp.jobDone.Store(true)
			}
		}
	}()
	go func() {
		rp.exitErr = cmd.Wait()
		pw.Close()
		close(rp.done)
	}()
	return rp, nil
}

func (rp *runnerProcess) started() bool {
	if rp.jobSeen.Load() {
		return true
	}
	if _, err := os.Stat(rp.stateFile); err == nil {
		rp.jobSeen.Store(true)
		return true
	}
	return false
}

// idleLogDue says when to upload the log while the runner waits for its job:
// after a minute, then every 5 minutes. A machine terminated before it takes
// its job may upload nothing else.
func idleLogDue(start, uploaded, now time.Time) bool {
	if uploaded.IsZero() {
		return now.Sub(start) >= time.Minute
	}
	return now.Sub(uploaded) >= 5*time.Minute
}

// superviseRunner waits for the runner, enforcing the idle and max lifetimes.
func (a *Agent) superviseRunner(ctx context.Context, rp *runnerProcess) {
	idle := time.Duration(a.Cfg.IdleMinutes) * time.Minute
	if idle <= 0 {
		idle = 15 * time.Minute
	}
	maxLife := time.Duration(a.Cfg.MaxMinutes) * time.Minute
	if maxLife <= 0 {
		maxLife = 12 * time.Hour
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	start := time.Now()
	var uploaded time.Time
	for {
		select {
		case <-rp.done:
			a.Log.Info("runner exited", "err", rp.exitErr, "ranJob", rp.started())
			return
		case <-ctx.Done():
			// The machine shuts down: upload first, it may not get to the end.
			a.Log.Warn("agent stopped, stopping the runner", "ranJob", rp.started())
			uctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			a.uploadLog(uctx)
			cancel()
			killTree(rp.cmd)
			<-rp.done
			return
		case <-t.C:
			if !rp.started() && idleLogDue(start, uploaded, time.Now()) {
				uploaded = time.Now()
				uctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				a.uploadLog(uctx)
				cancel()
			}
			if !rp.started() && time.Since(start) > idle {
				a.warn(fmt.Sprintf("no job picked up within %s; shutting down", idle))
				killTree(rp.cmd)
				select {
				case <-rp.done:
				case <-time.After(30 * time.Second):
				}
				return
			}
			if time.Since(start) > maxLife {
				a.warn(fmt.Sprintf("maximum lifetime of %s reached; stopping the runner", maxLife))
				killTree(rp.cmd)
				select {
				case <-rp.done:
				case <-time.After(30 * time.Second):
				}
				return
			}
		}
	}
}

// keepAlive holds the machine after the job for debugging, until the time
// runs out or someone creates the release file.
func (a *Agent) keepAlive(ctx context.Context) {
	if a.Cfg.KeepMinutes <= 0 {
		return
	}
	release := filepath.Join(stateDir(), "done")
	a.Log.Info("keeping machine for debugging", "minutes", a.Cfg.KeepMinutes, "release", "touch "+release)
	deadline := time.Now().Add(time.Duration(a.Cfg.KeepMinutes) * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(release); err == nil {
			a.Log.Info("released early")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}
