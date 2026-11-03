package agent

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/codician-team/truner/internal/agentcfg"
	"github.com/codician-team/truner/internal/cacheproxy"
	"github.com/codician-team/truner/internal/cachestore"
	"github.com/codician-team/truner/internal/github"
	"github.com/codician-team/truner/internal/version"
)

func (a *Agent) user() string {
	if a.Cfg.User != "" {
		return a.Cfg.User
	}
	return "runner"
}

// profileInstalled reports whether the machine already has the software of
// a profile (because it was baked into the AMI or a pool prepared it).
func profileInstalled(profile string) bool {
	data, err := os.ReadFile(bakedMarker())
	if err != nil {
		return false
	}
	have := strings.TrimSpace(string(data))
	return have == profile || have == "full" && profile == "standard"
}

func (a *Agent) installProfileIfNeeded(ctx context.Context) error {
	profile := strings.TrimPrefix(a.Cfg.Profile, "baked:")
	if profile == "" || profile == "none" {
		return nil
	}
	if profileInstalled(profile) {
		a.Log.Info("software profile already installed", "profile", profile)
		return nil
	}
	if strings.HasPrefix(a.Cfg.Profile, "baked:") {
		a.warn("image was expected to be baked but has no profile marker; installing software now")
	} else if profile == "full" {
		a.warn("the full image is not baked yet (first use builds it in the background); installing tools on this machine, which takes several minutes")
	}
	if err := a.osInstallProfile(ctx, profile); err != nil {
		return err
	}
	return os.WriteFile(bakedMarker(), []byte(profile), 0o644)
}

// prepareMachine installs everything the runner needs.
func (a *Agent) prepareMachine(ctx context.Context) error {
	if a.Cfg.Mode == agentcfg.ModeRun {
		a.warmRunner(a.osRunnerDir())
	}
	if err := a.timed("prepare", func() error { return a.osPrepare(ctx) }); err != nil {
		a.warn("system preparation: " + err.Error())
	}
	if err := a.timed("user", func() error { return a.osEnsureUser(ctx) }); err != nil {
		return fmt.Errorf("create runner user: %w", err)
	}
	var perr, rerr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		perr = a.timed("software", func() error { return a.installProfileIfNeeded(ctx) })
	}()
	go func() {
		defer wg.Done()
		rerr = a.timed("runner", func() error { return a.ensureRunner(ctx, a.osRunnerDir()) })
	}()
	wg.Wait()
	if rerr != nil {
		return rerr
	}
	// Sequential, after the profile: both may use apt.
	a.osRunnerDeps(ctx, a.osRunnerDir())
	// After the runner is unpacked: local NVMe is mounted inside its work folder.
	if a.Cfg.UseNVMe {
		a.timed("storage", func() error {
			m, err := a.osSetupStorage(ctx)
			a.storage = m
			return err
		})
	}
	if perr != nil {
		if a.Cfg.Mode != agentcfg.ModeRun {
			return fmt.Errorf("install software: %w", perr)
		}
		a.warn("software installation had errors: " + perr.Error())
	}
	if err := a.timed("docker", func() error { return a.osConfigureDocker(ctx) }); err != nil {
		a.warn("docker setup: " + err.Error())
	}
	return nil
}

func (a *Agent) runJob(ctx context.Context) error {
	if !a.micro() {
		a.loadMetadata(ctx)
		sctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go a.watchSpot(sctx)
	}

	fatal := a.Cfg.FatalError
	dir := a.osRunnerDir()
	if fatal == "" && a.micro() {
		// The MicroVM image prepared everything; this only downloads a runner
		// when the job asks for another version than the image has.
		if err := a.timed("runner", func() error { return a.ensureRunner(ctx, dir) }); err != nil {
			fatal = "truner could not install the runner: " + err.Error()
		}
	} else if fatal == "" {
		if err := a.prepareMachine(ctx); err != nil {
			fatal = "truner could not prepare the machine: " + err.Error()
			a.Log.Error("machine preparation failed", "err", err)
		}
	}
	if fatal != "" {
		// Make sure we can still run the runner so the job fails visibly.
		if err := a.osEnsureUser(ctx); err != nil {
			return err
		}
		if err := a.ensureRunner(ctx, dir); err != nil {
			return err
		}
		a.osRunnerDeps(ctx, dir)
	}

	cacheOn := false
	if a.Cfg.Cache.Enabled && fatal == "" {
		err := a.timed("cache", func() error {
			if err := PatchWorker(dir); err != nil {
				return err
			}
			return a.startCache(ctx)
		})
		if err != nil {
			a.warn("magic cache disabled, using GitHub's cache service: " + err.Error())
		} else {
			cacheOn = true
		}
	}
	if a.Cfg.ECRRegistry != "" && fatal == "" {
		if err := a.timed("ecr-login", func() error { return a.ecrLogin(ctx) }); err != nil {
			a.warn("ECR login failed: " + err.Error())
		}
	}
	if len(a.Cfg.SSHUsers) > 0 && fatal == "" {
		if err := a.timed("ssh", func() error { return a.installSSHKeys(ctx) }); err != nil {
			a.warn("SSH setup failed: " + err.Error())
		}
	}
	env, err := a.writeRunnerFiles(dir, cacheOn, fatal)
	if err != nil {
		return err
	}
	if err := a.osFinalizeFiles(dir); err != nil {
		return err
	}
	cmd := a.osRunnerCommand(dir, env)
	a.Log.Info("starting runner", "name", a.Cfg.RunnerName, "ready", time.Since(a.started).Round(time.Millisecond))
	rp, err := a.startRunner(cmd)
	if err != nil {
		return fmt.Errorf("start runner: %w", err)
	}
	a.superviseRunner(ctx, rp)
	a.keepAlive(ctx)
	if a.cache != nil {
		st := a.cache.Stats()
		a.Log.Info("cache stats", "hits", st.Hits, "misses", st.Misses, "saves", st.Saves, "down", st.BytesDown, "up", st.BytesUp)
	}
	return nil
}

func (a *Agent) bake(ctx context.Context) error {
	a.loadMetadata(ctx)
	a.Cfg.UseNVMe = false
	if err := a.prepareMachine(ctx); err != nil {
		return err
	}
	if err := os.WriteFile(bakedMarker(), []byte(strings.TrimPrefix(a.Cfg.Profile, "baked:")), 0o644); err != nil {
		return err
	}
	// Upload the full log now: cleanup below wipes logs from the image.
	a.uploadLog(ctx)
	a.Cfg.LogKey = ""
	if err := a.timed("cleanup", func() error { return a.osBakeCleanup(ctx) }); err != nil {
		return err
	}
	a.writeMarker(ctx, true, "")
	return nil
}

func (a *Agent) preparePool(ctx context.Context) error {
	a.loadMetadata(ctx)
	// Instance store is wiped when a machine stops, so NVMe is set up at claim time.
	a.Cfg.UseNVMe = false
	if err := a.prepareMachine(ctx); err != nil {
		return err
	}
	if err := a.osEnablePoolService(); err != nil {
		return err
	}
	if err := os.WriteFile(poolMarker(), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
		return err
	}
	a.writeMarker(ctx, true, "")
	return nil
}

// Boot is the entry point of the boot unit on warm pool machines and machines
// from baked images: it runs the job in the instance's user data.
func Boot(ctx context.Context, logPath string) int {
	ud, err := UserData(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "truner: read user data:", err)
		return 1
	}
	cfg, err := agentcfg.FromUserData(ud)
	if err != nil {
		fmt.Fprintln(os.Stderr, "truner: parse user data:", err)
		return 1
	}
	_, err = os.Stat(poolMarker())
	pooled := err == nil
	if cfg.Mode == agentcfg.ModePool && pooled {
		// Started without being claimed: go back to sleep.
		fmt.Fprintln(os.Stderr, "truner: pool machine started without a job, stopping")
		powerOff()
		return 0
	}
	if cfg.Mode == agentcfg.ModeRun && !pooled && cfg.Version != "" && cfg.Version != version.Version {
		// An image baked by an older truner: the user data script downloads the
		// agent this job needs and starts it.
		fmt.Fprintf(os.Stderr, "truner: this job needs agent %s, this image has %s; the user data script starts it\n", cfg.Version, version.Version)
		return 0
	}
	if !Lock() {
		fmt.Fprintln(os.Stderr, "truner: an agent is already running on this machine")
		return 0
	}
	a, _ := New(cfg, logPath)
	return a.Main(ctx)
}

func (a *Agent) startCache(ctx context.Context) error {
	cfg, err := a.aws(ctx)
	if err != nil {
		return err
	}
	ip := a.meta.PrivateIP
	if ip == "" {
		ip = "127.0.0.1"
	}
	tmp := filepath.Join(a.osScratchDir(), "cache-tmp")
	srv, err := cacheproxy.New(cacheproxy.Options{
		Store:     cachestore.NewS3(s3.NewFromConfig(cfg), a.Cfg.Cache.Bucket),
		Prefix:    a.Cfg.Cache.Prefix,
		PublicURL: fmt.Sprintf("http://%s:%d", ip, CachePort),
		TempDir:   tmp,
		Logger:    a.Log.With("component", "cache"),
	})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", CachePort))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/_truner/stats", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, cacheSummary(srv.Stats()))
	})
	mux.Handle("/", srv)
	go (&http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}).Serve(ln)
	a.cache = srv
	a.cacheURL = fmt.Sprintf("http://%s:%d/", ip, CachePort)
	go func() {
		for range time.Tick(30 * time.Minute) {
			srv.Cleanup(6 * time.Hour)
		}
	}()
	return nil
}

func cacheSummary(st cacheproxy.Stats) string {
	if st.Hits+st.Misses+st.Saves == 0 {
		return "truner magic cache: not used by this job"
	}
	s := fmt.Sprintf("truner magic cache: %d hit(s), %d miss(es), %d save(s)", st.Hits, st.Misses, st.Saves)
	if st.BytesDown > 0 && st.DownNanos > 0 {
		s += fmt.Sprintf(" · restored %s at %s/s", humanBytes(st.BytesDown), humanBytes(int64(float64(st.BytesDown)/time.Duration(st.DownNanos).Seconds())))
	}
	if st.BytesUp > 0 {
		s += fmt.Sprintf(" · saved %s", humanBytes(st.BytesUp))
	}
	if st.SaveErrors > 0 {
		s += fmt.Sprintf(" · %d save error(s)", st.SaveErrors)
	}
	return s
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func (a *Agent) ecrLogin(ctx context.Context) error {
	cfg, err := a.aws(ctx)
	if err != nil {
		return err
	}
	out, err := ecr.NewFromConfig(cfg).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return err
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return errors.New("no authorization data")
	}
	token := *out.AuthorizationData[0].AuthorizationToken
	dir := a.osDockerConfigDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "config.json")
	conf := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &conf)
	}
	auths, _ := conf["auths"].(map[string]any)
	if auths == nil {
		auths = map[string]any{}
	}
	auths[a.Cfg.ECRRegistry] = map[string]string{"auth": token}
	conf["auths"] = auths
	data, _ := json.MarshalIndent(conf, "", "  ")
	return os.WriteFile(path, data, 0o600)
}

func (a *Agent) installSSHKeys(ctx context.Context) error {
	var keys []string
	for _, u := range a.Cfg.SSHUsers {
		k, err := github.UserKeys(ctx, u)
		if err != nil {
			a.warn("could not fetch SSH keys of " + u + ": " + err.Error())
			continue
		}
		keys = append(keys, k...)
	}
	if len(keys) == 0 {
		return errors.New("no SSH keys found")
	}
	return a.osInstallSSHKeys(keys)
}

// writeRunnerFiles writes the runner's .env, the job hooks and the banner.
func (a *Agent) writeRunnerFiles(dir string, cacheOn bool, fatal string) ([]string, error) {
	sd := stateDir()
	if err := os.MkdirAll(sd, 0o775); err != nil {
		return nil, err
	}
	env := map[string]string{
		"LANG":                              "C.UTF-8",
		"ImageOS":                           a.Cfg.ImageOS,
		"ImageVersion":                      "truner-" + a.Cfg.Version,
		"RUNNER_TOOL_CACHE":                 a.osToolCache(),
		"AGENT_TOOLSDIRECTORY":              a.osToolCache(),
		"ACTIONS_RUNNER_HOOK_JOB_STARTED":   a.hookPath("job-started"),
		"ACTIONS_RUNNER_HOOK_JOB_COMPLETED": a.hookPath("job-completed"),
		"AWS_REGION":                        a.Cfg.Region,
		"AWS_DEFAULT_REGION":                a.Cfg.Region,
		"TRUNER":                            "true",
		"TRUNER_STACK":                      a.Cfg.Stack,
		"TRUNER_INSTANCE_ID":                a.meta.InstanceID,
		"TRUNER_INSTANCE_TYPE":              a.meta.InstanceType,
		"TRUNER_INSTANCE_LIFECYCLE":         a.meta.Lifecycle,
		"TRUNER_AZ":                         a.meta.AZ,
		"TRUNER_S3_BUCKET":                  a.Cfg.Bucket,
	}
	if a.Cfg.ImageOS == "" {
		delete(env, "ImageOS")
	}
	if a.Cfg.Cache.Prefix != "" {
		// Ready-made prefix for buildx `type=s3` caches of this repository.
		env["TRUNER_S3_DOCKER_PREFIX"] = "docker/" + strings.TrimPrefix(a.Cfg.Cache.Prefix, "cache/")
	}
	if a.Cfg.ECRRepo != "" {
		env["TRUNER_ECR_CACHE"] = a.Cfg.ECRRepo
	}
	if a.storage != "" {
		env["TRUNER_NVME"] = a.storage
	}
	if a.Cfg.Platform != "windows" {
		env["DEBIAN_FRONTEND"] = "noninteractive"
	}
	if a.user() == "root" {
		env["RUNNER_ALLOW_RUNASROOT"] = "1"
	}
	for k, v := range readKV(extraEnvFile()) {
		env[k] = v
	}
	if a.micro() {
		// The execution role's credentials reach the guest through AWS_*
		// settings rather than IMDS; jobs need them for the AWS CLI and SDKs.
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "AWS_") && k != "AWS_REGION" && k != "AWS_DEFAULT_REGION" {
				env[k] = v
			}
		}
	}
	env["PATH"] = a.osBasePath(readLines(extraPathFile()))
	var jobEnv []string
	if cacheOn {
		env["ACTIONS_RESULTS_URL"] = a.cacheURL
		env["ACTIONS_CACHE_URL"] = a.cacheURL
		jobEnv = append(jobEnv, "ACTIONS_RESULTS_URL="+a.cacheURL, "ACTIONS_CACHE_URL="+a.cacheURL)
	}
	for k, v := range a.Cfg.Env {
		env[k] = v
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lines []string
	for _, k := range keys {
		if strings.ContainsAny(env[k], "\r\n") {
			continue
		}
		lines = append(lines, k+"="+env[k])
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(sd, "job.env"), []byte(strings.Join(jobEnv, "\n")+"\n"), 0o644); err != nil {
		return nil, err
	}
	fatalPath := filepath.Join(sd, "fatal.txt")
	os.Remove(fatalPath)
	if fatal != "" {
		if err := os.WriteFile(fatalPath, []byte(strings.ReplaceAll(fatal, "\n", " ")), 0o644); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(sd, "banner.txt"), []byte(a.banner(cacheOn)), 0o644); err != nil {
		return nil, err
	}
	if err := a.osWriteHooks(); err != nil {
		return nil, err
	}
	return lines, nil
}

// banner is printed at the top of every job by the job-started hook.
func (a *Agent) banner(cacheOn bool) string {
	var b strings.Builder
	m := a.meta
	life := m.Lifecycle
	if life == "" {
		life = "on-demand"
	}
	cpus, mem := machineSpecs()
	ready := time.Since(a.started) + a.uptime
	summary := fmt.Sprintf("truner | %s %s in %s | %d vCPU, %.0f GiB | ready %ds after boot", m.InstanceType, life, m.AZ, cpus, mem, int(ready.Seconds()))
	machine := fmt.Sprintf("%s (%s) %s in %s", m.InstanceType, life, m.InstanceID, m.AZ)
	if a.micro() {
		v := max(a.Cfg.MicroVCPU, 1)
		summary = fmt.Sprintf("truner | Lambda MicroVM in %s | %d vCPU, %d GiB, bursting to %d vCPU, %d GiB | ready %.1fs after start", a.Cfg.Region, v, 2*v, 4*v, 8*v, ready.Seconds())
		machine = fmt.Sprintf("Lambda MicroVM %s in %s (linux arm64)", m.InstanceID, a.Cfg.Region)
	}
	if a.Cfg.Job.QueuedAt > 0 {
		summary += fmt.Sprintf(", %ds after the job was queued", time.Now().Unix()-a.Cfg.Job.QueuedAt)
	}
	b.WriteString(summary + "\n")
	b.WriteString("::group::truner runner details\n")
	fmt.Fprintf(&b, "Machine:    %s\n", machine)
	if a.Cfg.Job.Spec != "" {
		fmt.Fprintf(&b, "Requested:  %s\n", a.Cfg.Job.Spec)
	}
	img := a.Cfg.ImageName
	if a.micro() {
		img += " (MicroVM snapshot)"
	} else if strings.HasPrefix(a.Cfg.Profile, "baked:") {
		img += " (baked image " + m.AMI + ")"
	} else {
		img += " (" + m.AMI + ")"
	}
	fmt.Fprintf(&b, "Image:      %s, actions/runner %s, truner %s\n", img, a.Cfg.RunnerVersion, a.Cfg.Version)
	if a.storage != "" {
		fmt.Fprintf(&b, "Disk:       local NVMe at %s (work directory and Docker)\n", a.storage)
	}
	if cacheOn {
		fmt.Fprintf(&b, "Cache:      truner magic cache (S3 bucket %s) for actions/cache, setup-* caching and buildx type=gha\n", a.Cfg.Cache.Bucket)
	} else if a.Cfg.Cache.Enabled {
		b.WriteString("Cache:      GitHub cache service (magic cache unavailable on this machine)\n")
	}
	if a.Cfg.ECRRepo != "" {
		fmt.Fprintf(&b, "Docker:     registry cache available at $TRUNER_ECR_CACHE (%s)\n", a.Cfg.ECRRepo)
	}
	var tl []string
	for _, s := range a.timeline {
		if s.took >= 100*time.Millisecond {
			tl = append(tl, fmt.Sprintf("%s %.1fs", s.name, s.took.Seconds()))
		}
	}
	if a.micro() {
		fmt.Fprintf(&b, "Boot:       resumed from a snapshot, agent %.1fs [%s]\n", time.Since(a.started).Seconds(), strings.Join(tl, ", "))
	} else {
		fmt.Fprintf(&b, "Boot:       machine %.0fs, agent %.1fs [%s]\n", a.uptime.Seconds(), time.Since(a.started).Seconds(), strings.Join(tl, ", "))
	}
	if len(a.Cfg.Job.Labels) > 0 {
		fmt.Fprintf(&b, "Labels:     %s\n", strings.Join(a.Cfg.Job.Labels, ", "))
	}
	if len(a.Cfg.SSHUsers) > 0 && m.PublicIP != "" {
		fmt.Fprintf(&b, "SSH:        ssh %s@%s (keys of %s)\n", a.user(), m.PublicIP, strings.Join(a.Cfg.SSHUsers, ", "))
	}
	if a.Cfg.KeepMinutes > 0 {
		fmt.Fprintf(&b, "Debug:      machine stays up %d min after the job; release it early with: touch %s\n", a.Cfg.KeepMinutes, filepath.Join(stateDir(), "done"))
	}
	b.WriteString("::endgroup::\n")
	for _, w := range append(append([]string{}, a.Cfg.Job.Warnings...), a.warnings...) {
		fmt.Fprintf(&b, "::warning title=truner::%s\n", strings.ReplaceAll(w, "\n", " "))
	}
	return b.String()
}

func readLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

func readKV(path string) map[string]string {
	out := map[string]string{}
	for _, l := range readLines(path) {
		if k, v, ok := strings.Cut(l, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// DecodeConfigFile reads an agent config file (base64 gzip JSON).
func DecodeConfigFile(path string) (*agentcfg.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(data))
	if strings.HasPrefix(s, "{") {
		c := &agentcfg.Config{}
		return c, json.Unmarshal([]byte(s), c)
	}
	if _, err := base64.StdEncoding.DecodeString(s); err != nil {
		return nil, fmt.Errorf("config is neither JSON nor base64: %w", err)
	}
	return agentcfg.Decode(s)
}
