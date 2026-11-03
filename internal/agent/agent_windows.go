//go:build windows

package agent

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed profiles/windows.ps1
var windowsProfile string

const winRoot = `C:\truner`

func stateDir() string      { return filepath.Join(winRoot, "run") }
func bakedMarker() string   { return filepath.Join(winRoot, "baked") }
func poolMarker() string    { return filepath.Join(winRoot, "pool-prepared") }
func extraEnvFile() string  { return filepath.Join(winRoot, "extra-env") }
func extraPathFile() string { return filepath.Join(winRoot, "extra-path") }

func extraLogFiles() []string { return []string{filepath.Join(winRoot, "bootstrap.log")} }

func machineUptime() time.Duration {
	ms, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetTickCount64").Call()
	return time.Duration(ms) * time.Millisecond
}

func machineSpecs() (int, float64) {
	type memStatus struct {
		Length               uint32
		MemoryLoad           uint32
		TotalPhys            uint64
		AvailPhys            uint64
		TotalPageFile        uint64
		AvailPageFile        uint64
		TotalVirtual         uint64
		AvailVirtual         uint64
		AvailExtendedVirtual uint64
	}
	var m memStatus
	m.Length = uint32(unsafe.Sizeof(m))
	windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx").Call(uintptr(unsafe.Pointer(&m)))
	return runtime.NumCPU(), float64(m.TotalPhys) / (1 << 30)
}

func powerOff() {
	exec.Command("shutdown.exe", "/s", "/t", "0", "/f").Run()
}

func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	exec.Command("taskkill.exe", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}

func (a *Agent) osPrepare(ctx context.Context) error {
	return os.MkdirAll(winRoot, 0o755)
}

// Windows jobs run as the agent's account (LocalSystem), like the runner
// service on self-hosted Windows machines.
func (a *Agent) osEnsureUser(ctx context.Context) error {
	for _, d := range []string{stateDir(), `C:\hostedtoolcache\windows`} {
		os.MkdirAll(d, 0o755)
	}
	return nil
}

func (a *Agent) osSetupStorage(ctx context.Context) (string, error) { return "", nil }

func (a *Agent) osInstallProfile(ctx context.Context, profile string) error {
	path := filepath.Join(winRoot, "profile.ps1")
	if err := os.WriteFile(path, []byte(windowsProfile), 0o644); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path)
	w := &logWriter{log: a.Log, prefix: "profile: "}
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	w.Flush()
	return err
}

func (a *Agent) osConfigureDocker(ctx context.Context) error { return nil }

func (a *Agent) osRunnerDir() string { return `C:\actions-runner` }

func (a *Agent) osRunnerCommand(dir string, env []string) *exec.Cmd {
	cmd := exec.Command(filepath.Join(dir, "run.cmd"), "--jitconfig", a.Cfg.JITConfig)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	return cmd
}

func (a *Agent) osFinalizeFiles(dir string) error { return nil }

func (a *Agent) hookPath(name string) string { return filepath.Join(winRoot, "hooks", name+".ps1") }

func (a *Agent) osWriteHooks() error {
	os.MkdirAll(filepath.Join(winRoot, "hooks"), 0o755)
	sd := stateDir()
	started := fmt.Sprintf(`# truner job-started hook
New-Item -ItemType File -Force -Path "%[1]s\job-started" | Out-Null
if (Test-Path "%[1]s\banner.txt") { Get-Content "%[1]s\banner.txt" }
if ((Test-Path "%[1]s\fatal.txt") -and ((Get-Item "%[1]s\fatal.txt").Length -gt 0)) {
  Write-Output ("::error title=truner::" + (Get-Content "%[1]s\fatal.txt" -Raw))
  exit 1
}
if ($env:GITHUB_ENV -and (Test-Path "%[1]s\job.env")) {
  Get-Content "%[1]s\job.env" | Where-Object { $_ -ne "" } | Add-Content -Path $env:GITHUB_ENV -Encoding utf8
}
exit 0
`, sd)
	completed := fmt.Sprintf(`# truner job-completed hook
New-Item -ItemType File -Force -Path "%[1]s\job-completed" | Out-Null
try { (Invoke-WebRequest -UseBasicParsing -TimeoutSec 3 -Uri "http://127.0.0.1:%[2]d/_truner/stats").Content } catch {}
exit 0
`, sd, CachePort)
	if err := os.WriteFile(a.hookPath("job-started"), []byte(started), 0o644); err != nil {
		return err
	}
	return os.WriteFile(a.hookPath("job-completed"), []byte(completed), 0o644)
}

func (a *Agent) osInstallSSHKeys(keys []string) error {
	return fmt.Errorf("SSH access is only supported on Linux runners")
}

func (a *Agent) osDockerConfigDir() string {
	return filepath.Join(os.Getenv("USERPROFILE"), ".docker")
}

func (a *Agent) osBakeCleanup(ctx context.Context) error {
	for _, p := range []string{filepath.Join(winRoot, "config.b64"), filepath.Join(winRoot, "bootstrap.log"), stateDir()} {
		os.RemoveAll(p)
	}
	// Let EC2Launch run user data again on instances started from the image.
	ec2launch := `C:\Program Files\Amazon\EC2Launch\EC2Launch.exe`
	if _, err := os.Stat(ec2launch); err == nil {
		out, err := exec.CommandContext(ctx, ec2launch, "reset", "--block").CombinedOutput()
		a.Log.Info("ec2launch reset", "out", strings.TrimSpace(string(out)), "err", err)
		return err
	}
	return fmt.Errorf("EC2Launch v2 not found; cannot prepare the image")
}

func (a *Agent) osEnablePoolService() error {
	return fmt.Errorf("warm pools are only supported on Linux")
}

// osBasePath uses the machine PATH from the registry so tools installed by
// the profile (Git, PowerShell 7) are visible to jobs.
func (a *Agent) osBasePath(extra []string) string {
	path := os.Getenv("PATH")
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`, registry.QUERY_VALUE); err == nil {
		if v, _, err := k.GetStringValue("Path"); err == nil && v != "" {
			if exp, err := registry.ExpandString(v); err == nil {
				path = exp
			} else {
				path = v
			}
		}
		k.Close()
	}
	for _, p := range []string{`C:\Program Files\Git\bin`, `C:\Program Files\Git\cmd`, `C:\Program Files\PowerShell\7`} {
		if !strings.Contains(strings.ToLower(path), strings.ToLower(p)) {
			if _, err := os.Stat(p); err == nil {
				path += ";" + p
			}
		}
	}
	return strings.Join(append(extra, path), ";")
}

func (a *Agent) osToolCache() string  { return `C:\hostedtoolcache\windows` }
func (a *Agent) osScratchDir() string { return winRoot }
