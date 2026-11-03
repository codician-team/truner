// Package agent is the on-instance half of truner. It prepares the machine,
// starts the GitHub runner with a just-in-time config, serves the magic
// cache, enforces lifetimes and powers the machine off when done.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/codician-team/truner/internal/agentcfg"
	"github.com/codician-team/truner/internal/cacheproxy"
)

// CachePort is where the magic cache proxy listens.
const CachePort = 28777

// Agent runs one machine lifecycle.
type Agent struct {
	Cfg *agentcfg.Config
	Log *slog.Logger
	// NoShutdown keeps the machine running at the end (local testing).
	NoShutdown bool

	logPath  string
	logFile  *os.File
	started  time.Time
	uptime   time.Duration // machine uptime when the agent started
	meta     Metadata
	awsCfg   aws.Config
	awsOnce  sync.Once
	awsErr   error
	mu       sync.Mutex
	timeline []step
	warnings []string
	cache    *cacheproxy.Server
	cacheURL string
	storage  string // local NVMe mount, if any
}

type step struct {
	name string
	took time.Duration
}

// Metadata is what the agent knows about its machine.
type Metadata struct {
	InstanceID, InstanceType, AZ, Region, PrivateIP, PublicIP, Lifecycle, AMI string
}

// New creates an agent logging to logPath (and stderr).
func New(cfg *agentcfg.Config, logPath string) (*Agent, error) {
	a := &Agent{Cfg: cfg, started: time.Now(), logPath: logPath, uptime: machineUptime()}
	var w io.Writer = os.Stderr
	if logPath != "" {
		os.MkdirAll(filepath.Dir(logPath), 0o755)
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		if err == nil {
			a.logFile = f
			w = io.MultiWriter(os.Stderr, f)
		}
	}
	a.Log = slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return a, nil
}

// Main runs the configured mode and always ends by powering off the machine
// (unless NoShutdown is set). It returns the process exit code.
func (a *Agent) Main(ctx context.Context) (code int) {
	a.Log.Info("truner agent starting", "mode", a.Cfg.Mode, "version", a.Cfg.Version, "platform", a.Cfg.Platform, "uptime", a.uptime.Round(time.Second))
	stop := a.Cfg.Mode == agentcfg.ModeBake || a.Cfg.Mode == agentcfg.ModePool
	defer func() {
		if r := recover(); r != nil {
			a.Log.Error("agent panic", "panic", r, "stack", string(debug.Stack()))
			code = 2
			if a.Cfg.MarkerKey != "" {
				a.writeMarker(context.Background(), false, fmt.Sprint(r))
			}
		}
		a.finish(stop)
	}()
	var err error
	switch a.Cfg.Mode {
	case agentcfg.ModeRun:
		err = a.runJob(ctx)
	case agentcfg.ModeBake:
		err = a.bake(ctx)
	case agentcfg.ModePool:
		err = a.preparePool(ctx)
	default:
		err = fmt.Errorf("unknown mode %q", a.Cfg.Mode)
	}
	if err != nil {
		a.Log.Error("agent failed", "err", err)
		if a.Cfg.MarkerKey != "" {
			a.writeMarker(ctx, false, err.Error())
		}
		return 1
	}
	return 0
}

// finish uploads the log and powers the machine off.
func (a *Agent) finish(stop bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a.Log.Info("agent finished", "took", time.Since(a.started).Round(time.Second))
	a.uploadLog(ctx)
	if a.NoShutdown {
		a.Log.Info("not shutting down (--no-shutdown)")
		return
	}
	a.Log.Info("powering off", "stop", stop)
	if a.logFile != nil {
		a.logFile.Sync()
	}
	powerOff()
}

// timed runs fn and records how long it took for the boot timeline.
func (a *Agent) timed(name string, fn func() error) error {
	start := time.Now()
	err := fn()
	a.mu.Lock()
	a.timeline = append(a.timeline, step{name, time.Since(start)})
	a.mu.Unlock()
	a.Log.Info("step done", "step", name, "took", time.Since(start).Round(time.Millisecond), "err", err)
	return err
}

func (a *Agent) warn(msg string) {
	a.mu.Lock()
	a.warnings = append(a.warnings, msg)
	a.mu.Unlock()
	a.Log.Warn(msg)
}

func (a *Agent) aws(ctx context.Context) (aws.Config, error) {
	a.awsOnce.Do(func() {
		region := a.Cfg.Region
		if region == "" {
			region = a.meta.Region
		}
		a.awsCfg, a.awsErr = awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	})
	return a.awsCfg, a.awsErr
}

// loadMetadata reads instance facts from IMDS (best effort).
func (a *Agent) loadMetadata(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c := imds.New(imds.Options{})
	get := func(p string) string {
		out, err := c.GetMetadata(ctx, &imds.GetMetadataInput{Path: p})
		if err != nil {
			return ""
		}
		defer out.Content.Close()
		b, _ := io.ReadAll(out.Content)
		return strings.TrimSpace(string(b))
	}
	a.meta = Metadata{
		InstanceID:   get("instance-id"),
		InstanceType: get("instance-type"),
		AZ:           get("placement/availability-zone"),
		Region:       get("placement/region"),
		PrivateIP:    get("local-ipv4"),
		PublicIP:     get("public-ipv4"),
		Lifecycle:    get("instance-life-cycle"),
		AMI:          get("ami-id"),
	}
	a.Log.Info("instance metadata", "instance", a.meta.InstanceID, "type", a.meta.InstanceType, "az", a.meta.AZ, "lifecycle", a.meta.Lifecycle)
}

// watchSpot logs spot interruption notices (the control plane gets the
// EventBridge event and arranges a retry).
func (a *Agent) watchSpot(ctx context.Context) {
	if a.meta.Lifecycle != "spot" {
		return
	}
	c := imds.New(imds.Options{})
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			out, err := c.GetMetadata(ctx, &imds.GetMetadataInput{Path: "spot/instance-action"})
			if err != nil {
				continue
			}
			b, _ := io.ReadAll(out.Content)
			out.Content.Close()
			a.Log.Warn("SPOT INTERRUPTION NOTICE: this machine will be reclaimed in ~2 minutes", "action", string(b))
			os.WriteFile(filepath.Join(stateDir(), "spot-interruption"), b, 0o644)
			uctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			a.uploadLog(uctx)
			cancel()
			return
		}
	}
}

// UserData fetches this instance's user data from IMDS.
func UserData(ctx context.Context) ([]byte, error) {
	out, err := imds.New(imds.Options{}).GetUserData(ctx, &imds.GetUserDataInput{})
	if err != nil {
		return nil, err
	}
	defer out.Content.Close()
	return io.ReadAll(out.Content)
}

func (a *Agent) uploadLog(ctx context.Context) {
	if a.Cfg.LogKey == "" || a.Cfg.Bucket == "" {
		return
	}
	cfg, err := a.aws(ctx)
	if err != nil {
		return
	}
	var buf bytes.Buffer
	for _, p := range append([]string{a.logPath}, extraLogFiles()...) {
		data, err := os.ReadFile(p)
		if err != nil || len(data) == 0 {
			continue
		}
		if len(data) > 8<<20 {
			data = data[len(data)-8<<20:]
		}
		fmt.Fprintf(&buf, "===== %s =====\n", p)
		buf.Write(data)
		buf.WriteString("\n")
	}
	_, err = s3.NewFromConfig(cfg).PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(a.Cfg.Bucket), Key: aws.String(a.Cfg.LogKey), Body: bytes.NewReader(buf.Bytes()),
		ContentType: aws.String("text/plain; charset=utf-8"),
	})
	if err != nil {
		a.Log.Warn("log upload failed", "err", err)
	}
}

// writeMarker reports bake/pool preparation results to the control plane.
func (a *Agent) writeMarker(ctx context.Context, ok bool, msg string) {
	if a.Cfg.MarkerKey == "" {
		return
	}
	cfg, err := a.aws(ctx)
	if err != nil {
		a.Log.Error("cannot write marker", "err", err)
		return
	}
	body, _ := json.Marshal(map[string]any{"ok": ok, "error": msg, "runnerVersion": a.Cfg.RunnerVersion, "instanceId": a.meta.InstanceID, "at": time.Now().UTC()})
	_, err = s3.NewFromConfig(cfg).PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(a.Cfg.Bucket), Key: aws.String(a.Cfg.MarkerKey), Body: bytes.NewReader(body)})
	if err != nil {
		a.Log.Error("marker upload failed", "err", err)
	}
}
