//go:build linux

package agent

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed profiles/standard.sh
var standardProfile string

//go:embed profiles/full.sh
var fullProfile string

func stateDir() string      { return "/var/lib/truner/run" }
func bakedMarker() string   { return "/etc/truner/baked" }
func poolMarker() string    { return "/etc/truner/pool-prepared" }
func extraEnvFile() string  { return "/etc/truner/extra-env" }
func extraPathFile() string { return "/etc/truner/extra-path" }

func extraLogFiles() []string {
	return []string{"/var/log/truner/bootstrap.log", "/var/log/cloud-init-output.log"}
}

func machineUptime() time.Duration {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f, err := strconv.ParseFloat(strings.Fields(string(data))[0], 64)
	if err != nil {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

func machineSpecs() (int, float64) {
	mem := 0.0
	for _, l := range readLines("/proc/meminfo") {
		if strings.HasPrefix(l, "MemTotal:") {
			f := strings.Fields(l)
			if len(f) >= 2 {
				kb, _ := strconv.ParseFloat(f[1], 64)
				mem = kb / 1024 / 1024
			}
		}
	}
	return runtime.NumCPU(), mem
}

func powerOff() {
	exec.Command("sync").Run()
	if err := exec.Command("systemctl", "poweroff").Run(); err != nil {
		exec.Command("shutdown", "-h", "now").Run()
	}
}

func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	syscall.Kill(-pgid, syscall.SIGINT)
	go func() {
		time.Sleep(25 * time.Second)
		syscall.Kill(-pgid, syscall.SIGKILL)
	}()
}

// sh runs a command, logging its output.
func (a *Agent) sh(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		a.Log.Info(name+" output", "out", strings.TrimSpace(tail(string(out), 4000)))
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func (a *Agent) osPrepare(ctx context.Context) error {
	// Background apt jobs grab the dpkg lock right after boot; runners are
	// disposable, so stop them.
	for _, u := range []string{"apt-daily.timer", "apt-daily-upgrade.timer", "unattended-upgrades.service", "apt-daily.service", "apt-daily-upgrade.service"} {
		exec.CommandContext(ctx, "systemctl", "stop", "--no-block", u).Run()
	}
	exec.Command("pkill", "-f", "unattended-upgrade").Run()
	os.MkdirAll("/etc/truner", 0o755)
	return nil
}

func (a *Agent) osEnsureUser(ctx context.Context) error {
	name := a.user()
	if _, err := user.Lookup(name); err != nil {
		if err := a.sh(ctx, "useradd", "-m", "-s", "/bin/bash", "-u", "1001", name); err != nil {
			if err := a.sh(ctx, "useradd", "-m", "-s", "/bin/bash", name); err != nil {
				return err
			}
		}
	}
	for _, g := range []string{"sudo", "adm", "systemd-journal", "docker"} {
		if _, err := user.LookupGroup(g); err == nil {
			exec.CommandContext(ctx, "usermod", "-aG", g, name).Run()
		}
	}
	sudoers := fmt.Sprintf("%s ALL=(ALL) NOPASSWD:ALL\nDefaults:%s !requiretty\nDefaults env_keep += \"DEBIAN_FRONTEND\"\n", name, name)
	if err := os.WriteFile("/etc/sudoers.d/90-truner-runner", []byte(sudoers), 0o440); err != nil {
		return err
	}
	u, err := user.Lookup(name)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	for _, d := range []string{filepath.Join(u.HomeDir, "work"), "/opt/hostedtoolcache", stateDir()} {
		os.MkdirAll(d, 0o775)
		os.Chown(d, uid, gid)
	}
	return nil
}

func (a *Agent) homeDir() string {
	if u, err := user.Lookup(a.user()); err == nil {
		return u.HomeDir
	}
	return "/home/" + a.user()
}

// osSetupStorage formats local NVMe instance store (RAID 0 across disks)
// and moves the work directory and Docker onto it.
func (a *Agent) osSetupStorage(ctx context.Context) (string, error) {
	var devs []string
	entries, _ := filepath.Glob("/sys/block/nvme*n1")
	for _, e := range entries {
		model, _ := os.ReadFile(filepath.Join(e, "device", "model"))
		if strings.Contains(string(model), "Instance Storage") {
			devs = append(devs, "/dev/"+filepath.Base(e))
		}
	}
	if len(devs) == 0 {
		return "", nil
	}
	dev := devs[0]
	if len(devs) > 1 {
		args := append([]string{"--create", "/dev/md0", "--level=0", "--force", "--run", fmt.Sprintf("--raid-devices=%d", len(devs))}, devs...)
		if err := a.sh(ctx, "mdadm", args...); err == nil {
			dev = "/dev/md0"
		} else {
			a.Log.Warn("RAID setup failed, using the first NVMe disk only", "err", err)
		}
	}
	const mnt = "/mnt/truner"
	if err := a.sh(ctx, "mkfs.ext4", "-F", "-q", "-m", "0", "-E", "nodiscard,lazy_itable_init=1,lazy_journal_init=1", dev); err != nil {
		return "", err
	}
	os.MkdirAll(mnt, 0o755)
	if err := a.sh(ctx, "mount", "-o", "noatime,nodiscard", dev, mnt); err != nil {
		return "", err
	}
	u, err := user.Lookup(a.user())
	if err != nil {
		return "", err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	work := filepath.Join(mnt, "work")
	os.MkdirAll(work, 0o775)
	os.Chown(work, uid, gid)
	home := filepath.Join(u.HomeDir, "work")
	os.MkdirAll(home, 0o775)
	if err := a.sh(ctx, "mount", "--bind", work, home); err != nil {
		return "", err
	}
	tmp := filepath.Join(mnt, "tmp")
	os.MkdirAll(tmp, 0o1777)
	os.Chmod(tmp, 0o1777)
	a.Log.Info("local NVMe storage ready", "devices", devs, "mount", mnt)
	return mnt, nil
}

func (a *Agent) osInstallProfile(ctx context.Context, profile string) error {
	scripts := []string{standardProfile}
	if profile == "full" {
		scripts = append(scripts, fullProfile)
	}
	for i, s := range scripts {
		path := fmt.Sprintf("/var/lib/truner/profile-%d.sh", i)
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(s), 0o755); err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, "/bin/bash", path)
		cmd.Env = append(os.Environ(), "TRUNER_USER="+a.user(), "DEBIAN_FRONTEND=noninteractive", "HOME=/root")
		w := &logWriter{log: a.Log, prefix: "profile: "}
		cmd.Stdout, cmd.Stderr = w, w
		start := time.Now()
		err := cmd.Run()
		w.Flush()
		a.Log.Info("profile script finished", "profile", profile, "part", i, "took", time.Since(start).Round(time.Second), "err", err)
		if err != nil {
			return err
		}
	}
	return nil
}

// osConfigureDocker moves Docker and containerd data onto local NVMe.
func (a *Agent) osConfigureDocker(ctx context.Context) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil
	}
	exec.CommandContext(ctx, "usermod", "-aG", "docker", a.user()).Run()
	if a.storage == "" {
		return nil
	}
	exec.CommandContext(ctx, "systemctl", "stop", "docker.socket", "docker.service", "containerd.service").Run()
	for _, d := range []string{"docker", "containerd"} {
		src := filepath.Join(a.storage, d)
		dst := "/var/lib/" + d
		os.MkdirAll(src, 0o711)
		os.MkdirAll(dst, 0o711)
		// Carry over anything already there (e.g. images baked into the AMI).
		exec.CommandContext(ctx, "cp", "-a", dst+"/.", src+"/").Run()
		if err := a.sh(ctx, "mount", "--bind", src, dst); err != nil {
			return err
		}
	}
	return a.sh(ctx, "systemctl", "start", "containerd.service", "docker.service")
}

func (a *Agent) osRunnerDir() string { return filepath.Join(a.homeDir(), "actions-runner") }

func (a *Agent) osRunnerCommand(dir string, env []string) *exec.Cmd {
	cmd := exec.Command(filepath.Join(dir, "run.sh"), "--jitconfig", a.Cfg.JITConfig)
	cmd.Dir = dir
	u, err := user.Lookup(a.user())
	if err == nil {
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		var groups []uint32
		if ids, err := u.GroupIds(); err == nil {
			for _, g := range ids {
				if n, err := strconv.Atoi(g); err == nil {
					groups = append(groups, uint32(n))
				}
			}
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}}
		env = append(env, "HOME="+u.HomeDir, "USER="+u.Username, "LOGNAME="+u.Username, "SHELL=/bin/bash")
	}
	cmd.Env = env
	return cmd
}

func (a *Agent) osFinalizeFiles(dir string) error {
	return a.sh(context.Background(), "chown", "-R", a.user()+":"+a.user(), dir, stateDir())
}

func (a *Agent) hookPath(name string) string { return "/var/lib/truner/hooks/" + name + ".sh" }

func (a *Agent) osWriteHooks() error {
	os.MkdirAll("/var/lib/truner/hooks", 0o755)
	sd := stateDir()
	started := fmt.Sprintf(`#!/bin/bash
# truner job-started hook
touch %[1]s/job-started 2>/dev/null || true
cat %[1]s/banner.txt 2>/dev/null || true
if [ -s %[1]s/fatal.txt ]; then
  echo "::error title=truner::$(cat %[1]s/fatal.txt)"
  exit 1
fi
if [ -n "${GITHUB_ENV:-}" ] && [ -s %[1]s/job.env ]; then
  grep -v '^$' %[1]s/job.env >>"$GITHUB_ENV" || true
fi
exit 0
`, sd)
	completed := fmt.Sprintf(`#!/bin/bash
# truner job-completed hook
touch %[1]s/job-completed 2>/dev/null || true
curl -fsS -m 3 http://127.0.0.1:%[2]d/_truner/stats 2>/dev/null || true
exit 0
`, sd, CachePort)
	if err := os.WriteFile(a.hookPath("job-started"), []byte(started), 0o755); err != nil {
		return err
	}
	return os.WriteFile(a.hookPath("job-completed"), []byte(completed), 0o755)
}

func (a *Agent) osInstallSSHKeys(keys []string) error {
	home := a.homeDir()
	dir := filepath.Join(home, ".ssh")
	os.MkdirAll(dir, 0o700)
	f, err := os.OpenFile(filepath.Join(dir, "authorized_keys"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strings.Join(keys, "\n") + "\n")
	f.Close()
	if err != nil {
		return err
	}
	a.sh(context.Background(), "chown", "-R", a.user()+":"+a.user(), dir)
	exec.Command("systemctl", "start", "ssh").Run()
	return nil
}

func (a *Agent) osDockerConfigDir() string { return filepath.Join(a.homeDir(), ".docker") }

func (a *Agent) osBakeCleanup(ctx context.Context) error {
	cmds := [][]string{
		{"apt-get", "clean"},
		{"cloud-init", "clean", "--logs", "--seed"},
		{"journalctl", "--rotate"},
		{"journalctl", "--vacuum-time=1s"},
	}
	for _, c := range cmds {
		exec.CommandContext(ctx, c[0], c[1:]...).Run()
	}
	globs := []string{"/etc/ssh/ssh_host_*", "/tmp/*", "/var/tmp/*", "/var/log/truner/*", "/etc/truner/config.b64", "/var/lib/truner/profile-*.sh", stateDir() + "/*", "/root/.bash_history"}
	for _, g := range globs {
		matches, _ := filepath.Glob(g)
		for _, m := range matches {
			os.RemoveAll(m)
		}
	}
	// Make sure the runner user owns its files after root-run installs.
	a.osFinalizeFiles(a.osRunnerDir())
	exec.Command("sync").Run()
	return nil
}

const poolUnit = `[Unit]
Description=truner agent (warm pool machine)
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/truner-agent boot
LimitNOFILE=1048576
TasksMax=infinity

[Install]
WantedBy=multi-user.target
`

func (a *Agent) osEnablePoolService() error {
	if err := os.WriteFile("/etc/systemd/system/truner-agent.service", []byte(poolUnit), 0o644); err != nil {
		return err
	}
	exec.Command("systemctl", "daemon-reload").Run()
	return a.sh(context.Background(), "systemctl", "enable", "truner-agent.service")
}

func (a *Agent) osBasePath(extra []string) string {
	base := []string{filepath.Join(a.homeDir(), ".local/bin"), "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin", "/snap/bin"}
	return strings.Join(append(extra, base...), ":")
}

func (a *Agent) osToolCache() string { return "/opt/hostedtoolcache" }
func (a *Agent) osScratchDir() string {
	if a.storage != "" {
		return filepath.Join(a.storage, "tmp")
	}
	return "/var/lib/truner"
}
