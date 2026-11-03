//go:build !linux && !windows

package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// The agent only runs on Linux and Windows instances. These stubs let the
// package build elsewhere (for example on macOS developer machines).

var errUnsupported = errors.New("the truner agent runs on Linux and Windows only")

func stateDir() string             { return filepath.Join(os.TempDir(), "truner", "run") }
func bakedMarker() string          { return filepath.Join(os.TempDir(), "truner", "baked") }
func poolMarker() string           { return filepath.Join(os.TempDir(), "truner", "pool-prepared") }
func extraEnvFile() string         { return filepath.Join(os.TempDir(), "truner", "extra-env") }
func extraPathFile() string        { return filepath.Join(os.TempDir(), "truner", "extra-path") }
func extraLogFiles() []string      { return nil }
func machineUptime() time.Duration { return 0 }
func machineSpecs() (int, float64) { return runtime.NumCPU(), 0 }
func powerOff()                    {}
func killTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
}

func (a *Agent) osPrepare(ctx context.Context) error                  { return errUnsupported }
func (a *Agent) osEnsureUser(ctx context.Context) error               { return errUnsupported }
func (a *Agent) osSetupStorage(ctx context.Context) (string, error)   { return "", nil }
func (a *Agent) osInstallProfile(ctx context.Context, p string) error { return errUnsupported }
func (a *Agent) osConfigureDocker(ctx context.Context) error          { return nil }
func (a *Agent) osRunnerDir() string                                  { return filepath.Join(os.TempDir(), "truner", "actions-runner") }
func (a *Agent) osRunnerCommand(dir string, env []string) *exec.Cmd {
	return exec.Command(filepath.Join(dir, "run.sh"), "--jitconfig", a.Cfg.JITConfig)
}
func (a *Agent) osFinalizeFiles(dir string) error { return nil }
func (a *Agent) hookPath(name string) string {
	return filepath.Join(os.TempDir(), "truner", name+".sh")
}
func (a *Agent) osWriteHooks() error                     { return nil }
func (a *Agent) osInstallSSHKeys(keys []string) error    { return errUnsupported }
func (a *Agent) osDockerConfigDir() string               { return filepath.Join(os.TempDir(), "truner", ".docker") }
func (a *Agent) osBakeCleanup(ctx context.Context) error { return errUnsupported }
func (a *Agent) osEnablePoolService() error              { return errUnsupported }
func (a *Agent) osBasePath(extra []string) string        { return os.Getenv("PATH") }
func (a *Agent) osToolCache() string                     { return filepath.Join(os.TempDir(), "truner", "toolcache") }
func (a *Agent) osScratchDir() string                    { return filepath.Join(os.TempDir(), "truner") }
