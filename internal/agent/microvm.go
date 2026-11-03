package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codician-team/truner/internal/agentcfg"
)

// In a Lambda MicroVM the agent is the application. While Lambda builds the
// MicroVM image, the agent installs the runner and answers /ready, and Lambda
// snapshots the machine with the runner in memory. Every MicroVM resumes from
// that snapshot and receives its job's config in the /run hook, so the runner
// starts seconds after the launch.

// The hooks the MicroVM image enables, all under agentcfg.MicroHookPath.
const (
	MicroHookPort = agentcfg.MicroHookPort
	MicroHookPath = agentcfg.MicroHookPath
)

// Hook states.
const (
	hookPending int32 = iota
	hookOK
	hookFailed
)

type microVM struct {
	a        *Agent
	ctx      context.Context
	ready    atomic.Int32
	valid    atomic.Int32
	validate sync.Once
	started  atomic.Bool
	done     chan struct{}
}

// ServeMicroVM serves the MicroVM lifecycle hooks until ctx ends. cfgPath is
// the image's config: no job, but the runner version to install.
func ServeMicroVM(ctx context.Context, cfgPath, logPath string) int {
	cfg, err := DecodeConfigFile(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "truner-agent:", err)
		return 1
	}
	cfg.Mode = agentcfg.ModeMicro
	a, _ := New(cfg, logPath)
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", MicroHookPort))
	if err != nil {
		a.Log.Error("cannot listen for MicroVM hooks", "err", err)
		return 1
	}
	m := &microVM{a: a, ctx: ctx, done: make(chan struct{})}
	go m.prepare()
	srv := &http.Server{Handler: m.handler(), ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	a.Log.Info("serving MicroVM hooks", "port", MicroHookPort, "version", cfg.Version)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		a.Log.Error("hook server failed", "err", err)
		return 1
	}
	return 0
}

// prepare runs during the image build, before the snapshot.
func (m *microVM) prepare() {
	if err := m.a.prepareMicroImage(m.ctx); err != nil {
		m.a.Log.Error("MicroVM image preparation failed", "err", err)
		m.ready.Store(hookFailed)
		return
	}
	m.a.Log.Info("MicroVM image ready for its snapshot", "took", time.Since(m.a.started).Round(time.Millisecond))
	m.ready.Store(hookOK)
}

func (a *Agent) prepareMicroImage(ctx context.Context) error {
	if err := a.timed("user", func() error { return a.osEnsureUser(ctx) }); err != nil {
		return fmt.Errorf("create runner user: %w", err)
	}
	dir := a.osRunnerDir()
	if err := a.timed("runner", func() error { return a.ensureRunner(ctx, dir) }); err != nil {
		return err
	}
	a.osRunnerDeps(ctx, dir)
	if err := PatchWorker(dir); err != nil {
		// Jobs try again and fall back to GitHub's cache service.
		a.warn("magic cache patch: " + err.Error())
	}
	if err := a.osFinalizeFiles(dir); err != nil {
		return err
	}
	// The snapshot keeps the page cache, so the files read here are already
	// in memory when a MicroVM starts.
	paths := []string{filepath.Join(dir, "bin")}
	if node := newestNode(filepath.Join(dir, "externals")); node != "" {
		paths = append(paths, filepath.Join(node, "bin", "node"))
	}
	n, b, took := prefetch(paths, 16)
	a.Log.Info("prefetched runner files", "files", n, "mib", b>>20, "took", took.Round(time.Millisecond))
	return nil
}

func status(ok int32) int {
	switch ok {
	case hookOK:
		return http.StatusOK
	case hookFailed:
		return http.StatusInternalServerError
	}
	// Not done yet: Lambda asks again until the hook's timeout.
	return http.StatusServiceUnavailable
}

func (m *microVM) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+MicroHookPath+"ready", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status(m.ready.Load()))
	})
	mux.HandleFunc("POST "+MicroHookPath+"validate", func(w http.ResponseWriter, r *http.Request) {
		// Runs on a MicroVM started from the new image. Lambda records which
		// parts of the snapshot this touches and fetches them first later.
		m.validate.Do(func() { go m.validateImage() })
		w.WriteHeader(status(m.valid.Load()))
	})
	mux.HandleFunc("POST "+MicroHookPath+"run", m.run)
	mux.HandleFunc("POST "+MicroHookPath+"terminate", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		m.a.Log.Info("MicroVM terminating")
		m.a.uploadLog(ctx)
		w.WriteHeader(http.StatusOK)
	})
	for _, h := range []string{"resume", "suspend"} {
		mux.HandleFunc("POST "+MicroHookPath+h, func(w http.ResponseWriter, r *http.Request) {
			m.a.Log.Info("MicroVM lifecycle event", "hook", h)
			w.WriteHeader(http.StatusOK)
		})
	}
	return mux
}

func (m *microVM) validateImage() {
	dir := m.a.osRunnerDir()
	ctx, cancel := context.WithTimeout(m.ctx, 2*time.Minute)
	defer cancel()
	cmds := [][]string{{filepath.Join(dir, "bin", "Runner.Listener"), "--version"}}
	if node := newestNode(filepath.Join(dir, "externals")); node != "" {
		cmds = append(cmds, []string{filepath.Join(node, "bin", "node"), "--version"})
	}
	for _, c := range cmds {
		out, err := exec.CommandContext(ctx, c[0], c[1:]...).CombinedOutput()
		if err != nil {
			m.a.Log.Error("MicroVM image validation failed", "cmd", c[0], "err", err, "out", tail(string(out), 500))
			m.valid.Store(hookFailed)
			return
		}
	}
	m.valid.Store(hookOK)
}

// runConfig reads the job's config from the run hook payload: a URL to it,
// since Lambda limits the payload to 4096 characters, or the config itself.
func runConfig(ctx context.Context, payload string) (*agentcfg.Config, error) {
	if !strings.HasPrefix(payload, "https://") && !strings.HasPrefix(payload, "http://") {
		return agentcfg.Decode(payload)
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, payload, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			data, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			switch {
			case rerr != nil:
				err = rerr
			case resp.StatusCode >= 400 && resp.StatusCode < 500:
				cancel()
				return nil, fmt.Errorf("GET config: %s", resp.Status) // expired or denied: final
			case resp.StatusCode != http.StatusOK:
				err = fmt.Errorf("GET config: %s", resp.Status)
			default:
				cancel()
				return agentcfg.Decode(string(data))
			}
		}
		cancel()
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return nil, lastErr
}

// run starts the job a MicroVM was launched for.
func (m *microVM) run(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MicrovmID string `json:"microvmId"`
		Payload   string `json:"runHookPayload"`
	}
	data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(data, &body); err != nil {
		http.Error(w, "bad run request: "+err.Error(), http.StatusBadRequest)
		return
	}
	cfg, err := runConfig(r.Context(), body.Payload)
	if err != nil {
		m.a.Log.Error("bad run hook payload", "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Job steps can reach this port too: only the first call counts.
	if !m.started.CompareAndSwap(false, true) {
		http.Error(w, "this MicroVM already runs a job", http.StatusConflict)
		return
	}
	a := m.a
	cfg.Mode = agentcfg.ModeMicro
	a.Cfg = cfg
	a.started, a.uptime = time.Now(), 0
	a.timeline, a.warnings = nil, nil
	a.meta = Metadata{InstanceID: body.MicrovmID, InstanceType: "microvm", Region: cfg.Region, Lifecycle: "microvm"}
	a.Log.Info("MicroVM started for a job", "microvm", body.MicrovmID, "runner", cfg.RunnerName, "job", cfg.Job.JobID)
	go func() {
		defer close(m.done)
		code := a.Main(m.ctx)
		a.Log.Info("job over; the control plane terminates this MicroVM", "code", code)
	}()
	w.WriteHeader(http.StatusOK)
}
