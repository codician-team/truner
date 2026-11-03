package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/truner/truner/internal/agentcfg"
	"github.com/truner/truner/internal/cachestore"
	"github.com/truner/truner/internal/config"
	"github.com/truner/truner/internal/github"
	"github.com/truner/truner/internal/labels"
)

// maxLaunchAttempts bounds retries for capacity and transient errors (~1 hour).
const maxLaunchAttempts = 15

// retryError asks for the job to be retried after a delay.
type retryError struct {
	err      error
	delay    time.Duration
	noCharge bool // waiting on a limit, not a failure: do not count the attempt
}

func (e *retryError) Error() string { return e.err.Error() }
func (e *retryError) Unwrap() error { return e.err }

// HandleQueueMessage processes one worker queue message.
func (cp *ControlPlane) HandleQueueMessage(ctx context.Context, body string) error {
	var m queueMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil || m.JobID == 0 {
		cp.Log.Error("dropping malformed queue message", "body", body)
		return nil
	}
	return cp.ProcessJob(ctx, m.JobID)
}

// ProcessJob launches a machine for a queued job.
func (cp *ControlPlane) ProcessJob(ctx context.Context, jobID int64) error {
	log := cp.Log.With("job", jobID)
	job, err := cp.Store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if job == nil {
		log.Warn("job record not found")
		return nil
	}
	stale := time.Now().Add(-5 * time.Minute).Unix()
	if job.Status == StatusLaunching && job.UpdatedAt > stale {
		return nil // another worker is on it
	}
	if job.Status != StatusQueued && job.Status != StatusLaunching {
		return nil // already launched, started, cancelled...
	}
	err = cp.Store.UpdateJob(ctx, jobID, map[string]any{"status": StatusLaunching}, StatusIn(job.Status))
	if errors.Is(err, ErrConditionFailed) {
		return nil
	}
	if err != nil {
		return err
	}
	log = log.With("repo", job.FullName())
	lerr := cp.launchForJob(ctx, job)
	if lerr == nil {
		return nil
	}
	var re *retryError
	if errors.As(lerr, &re) {
		attempts := job.Attempts
		if !re.noCharge {
			attempts++
		}
		if attempts >= maxLaunchAttempts {
			log.Error("giving up on job", "attempts", attempts, "err", lerr)
			cp.emit("LaunchFailures", 1, UnitCount)
			cp.alert(ctx, "truner: could not start a runner", fmt.Sprintf("Job %s (%s / %s) could not get a machine after %d attempts: %v", job.FullName(), job.Workflow, job.Name, attempts, lerr))
			return ignoreCond(cp.Store.UpdateJob(ctx, jobID, map[string]any{"status": StatusFailed, "error": lerr.Error(), "attempts": attempts}, nil))
		}
		log.Warn("launch deferred", "err", lerr, "retryIn", re.delay, "attempt", attempts)
		if err := cp.Store.UpdateJob(ctx, jobID, map[string]any{"status": StatusQueued, "error": lerr.Error(), "attempts": attempts}, nil); err != nil {
			return err
		}
		return cp.enqueue(ctx, jobID, re.delay)
	}
	log.Error("launch failed permanently", "err", lerr)
	cp.emit("LaunchFailures", 1, UnitCount)
	return ignoreCond(cp.Store.UpdateJob(ctx, jobID, map[string]any{"status": StatusFailed, "error": lerr.Error()}, nil))
}

func backoff(attempt int) time.Duration {
	d := time.Duration(15*(1<<min(attempt, 5))) * time.Second
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

func (cp *ControlPlane) launchForJob(ctx context.Context, job *Job) error {
	gh, err := cp.GitHub(ctx)
	if err != nil {
		return &retryError{err: err, delay: time.Minute}
	}
	g := cp.GlobalConfig(ctx)
	if !g.Access.RepoAllowed(job.FullName()) {
		cp.Log.Warn("repository not allowed by access rules", "repo", job.FullName())
		return ignoreCond(cp.Store.UpdateJob(ctx, job.JobID, map[string]any{"status": StatusRejected, "error": "repository not allowed by access rules"}, nil))
	}

	// Resolve the machine. A bad config or label still gets a (tiny) runner
	// that fails the job with a readable error instead of leaving it queued.
	var fatal string
	repoCfg, err := cp.RepoConfig(ctx, gh, job)
	if err != nil {
		fatal = err.Error()
		repoCfg = &config.Repo{}
	}
	lbl, _ := labels.Parse(cp.Env.LabelPrefix, job.Labels)
	res := config.Resolver{Global: g, Repo: repoCfg}
	spec, err := res.Resolve(lbl)
	if err == nil && fatal != "" {
		err = errors.New(fatal)
	}
	if err != nil {
		fatal = "truner could not start a runner for this job: " + err.Error()
		spec, _ = config.Resolver{}.Resolve(labels.Labels{Values: map[string]string{"cpu": "2", "ram": "1"}})
		spec.Cache = config.CacheOff
	}

	// Safety rails.
	if limit := g.Limits.MonthlyBudgetUSD; limit > 0 && fatal == "" {
		if spent := cp.MonthSpend(ctx); spent >= limit {
			return &retryError{err: fmt.Errorf("monthly budget of $%.2f exhausted ($%.2f spent)", limit, spent), delay: 10 * time.Minute, noCharge: true}
		}
	}
	maxInst := g.Limits.MaxInstances
	if maxInst == 0 {
		maxInst = config.DefaultMaxInstance
	}
	if running, err := cp.runningCount(ctx); err == nil && running >= maxInst {
		return &retryError{err: fmt.Errorf("instance limit reached (%d/%d)", running, maxInst), delay: 30 * time.Second, noCharge: true}
	}

	forceOD := job.ForceOD
	if !forceOD {
		if it, _ := cp.Store.GetItem(ctx, fmt.Sprintf("ondemand#%s#%d", job.FullName(), job.RunID)); it != nil {
			forceOD = true
		}
	}

	rel, err := cp.RunnerRelease(ctx)
	if err != nil {
		return &retryError{err: err, delay: 30 * time.Second}
	}
	runnerName := fmt.Sprintf("truner-%d-%s", job.JobID, randHex(3))

	// Warm pools: start a stopped, pre-provisioned machine.
	if spec.Pool != "" && fatal == "" {
		r, err := cp.claimPoolInstance(ctx, gh, job, spec, runnerName, rel)
		if err != nil {
			cp.Log.Warn("pool claim failed, launching fresh", "pool", spec.Pool, "err", err)
		}
		if r != nil {
			return cp.recordLaunch(ctx, job, spec, r, runnerName, 0, nil, true)
		}
	}

	img, err := cp.ResolveImage(ctx, spec.Image, spec.ImageSpec, spec.Arch, true)
	if err != nil {
		return &retryError{err: err, delay: backoff(job.Attempts)}
	}
	jit, err := gh.GenerateJITConfig(ctx, job.InstallationID, job.Owner, job.Repo, runnerName, job.Labels)
	if err != nil {
		var ge *github.Error
		if errors.As(err, &ge) && (ge.Status == 403 || ge.Status == 404) {
			return fmt.Errorf("cannot register a runner on %s (does the GitHub App have Administration: write on this repo?): %w", job.FullName(), err)
		}
		return &retryError{err: fmt.Errorf("generate runner config: %w", err), delay: backoff(job.Attempts)}
	}

	ac := cp.agentConfig(ctx, job, spec, img, runnerName, jit.EncodedJITConfig, rel, fatal)
	ud, err := cp.buildUserData(ctx, ac)
	if err != nil {
		gh.DeleteRunner(ctx, job.InstallationID, job.Owner, job.Repo, jit.Runner.ID)
		return err
	}
	tags := cp.runnerTags(job, spec, runnerName)
	r, err := cp.Launch(ctx, LaunchInput{
		Name: runnerName, Spec: spec, Image: img, UserData: ud,
		Subnets: cp.subnets(spec.Private), Tags: tags, ForceOnDemand: forceOD,
	})
	if err != nil {
		gh.DeleteRunner(ctx, job.InstallationID, job.Owner, job.Repo, jit.Runner.ID)
		if strings.Contains(err.Error(), "VcpuLimitExceeded") || strings.Contains(err.Error(), "MaxSpotInstanceCountExceeded") {
			cp.emit("QuotaErrors", 1, UnitCount)
			hour := time.Now().UTC().Format("2006-01-02T15")
			if ok, _ := cp.Store.PutItem(ctx, "alert#quota#"+hour, Item{}, 2*time.Hour, true); ok {
				cp.alert(ctx, "truner: EC2 vCPU quota reached", "Launching a runner failed because the account's EC2 vCPU quota is exhausted:\n"+err.Error()+
					"\n\nJobs wait and retry automatically. Raise the quota with `truner quota --request 512` (see `truner doctor`).")
			}
			return &retryError{err: err, delay: time.Minute}
		}
		var ce *CapacityError
		if errors.As(err, &ce) {
			cp.emit("CapacityErrors", 1, UnitCount)
		}
		return &retryError{err: err, delay: backoff(job.Attempts)}
	}
	if r.FellBack {
		cp.emit("SpotFallbacks", 1, UnitCount)
	}
	if err := cp.recordLaunch(ctx, job, spec, r, runnerName, jit.Runner.ID, img, false); err != nil {
		return err
	}
	// Only now, with the job's machine on its way, start maintaining an
	// optimized image so later jobs boot faster.
	cp.recordImageUse(ctx, spec.Image, spec.ImageSpec, spec.Arch)
	if !img.Baked && ImageBakeable(spec.ImageSpec) && config.BoolOr(g.AutoBake, true) && fatal == "" {
		if _, err := cp.EnsureBake(ctx, spec.Image, spec.ImageSpec, spec.Arch, false); err != nil {
			cp.Log.Warn("could not start image bake", "image", spec.Image, "err", err)
		}
	}
	return nil
}

func (cp *ControlPlane) recordLaunch(ctx context.Context, job *Job, spec *config.JobSpec, r *LaunchResult, runnerName string, runnerID int64, img *ResolvedImage, fromPool bool) error {
	if runnerID == 0 {
		runnerID = r.RunnerID
	}
	price, err := cp.HourlyPrice(ctx, r.InstanceType, r.AZ, r.Lifecycle, spec.Platform())
	if err != nil {
		cp.Log.Warn("price lookup failed", "type", r.InstanceType, "err", err)
	}
	set := map[string]any{
		"status": StatusLaunched, "runnerName": runnerName, "runnerId": runnerID, "instanceId": r.InstanceID,
		"instanceType": r.InstanceType, "lifecycle": r.Lifecycle, "az": r.AZ, "image": spec.Image, "platform": spec.Platform(),
		"diskGiB": spec.Disk, "price": price, "launchedAt": time.Now().Unix(), "fellBack": r.FellBack,
		"launchMillis": r.Took.Milliseconds(), "keepMinutes": spec.Keep, "error": "", "cpu": spec.CPU.Min,
	}
	if img != nil {
		set["ami"] = img.ID
	}
	if fromPool {
		set["pool"] = spec.Pool
	}
	cp.Log.Info("runner launched", "job", job.JobID, "repo", job.FullName(), "instance", r.InstanceID, "type", r.InstanceType,
		"lifecycle", r.Lifecycle, "az", r.AZ, "pool", fromPool, "tookMs", r.Took.Milliseconds(), "pricePerHour", price)
	cp.emit("InstancesLaunched", 1, UnitCount)
	cp.emit("LaunchLatency", float64(r.Took.Milliseconds()), UnitMillis)
	cp.emit("QueueToLaunchSeconds", float64(time.Now().Unix()-job.TS), UnitSeconds)
	return cp.Store.UpdateJob(ctx, job.JobID, set, nil)
}

// runningCount counts active machines, cached briefly so bursts of jobs do
// not each list every instance.
func (cp *ControlPlane) runningCount(ctx context.Context) (int, error) {
	cp.mu.Lock()
	if time.Since(cp.countAt) < 10*time.Second {
		n := cp.count
		cp.count++ // account for the machine this caller is about to start
		cp.mu.Unlock()
		return n, nil
	}
	cp.mu.Unlock()
	insts, err := cp.ManagedInstances(ctx, "pending", "running")
	if err != nil {
		return 0, err
	}
	cp.mu.Lock()
	cp.count, cp.countAt = len(insts)+1, time.Now()
	cp.mu.Unlock()
	return len(insts), nil
}

func (cp *ControlPlane) subnets(private bool) []string {
	if private && len(cp.Env.PrivateSubnets) > 0 {
		return cp.Env.PrivateSubnets
	}
	if len(cp.Env.PublicSubnets) > 0 {
		return cp.Env.PublicSubnets
	}
	return cp.Env.PrivateSubnets
}

func (cp *ControlPlane) runnerTags(job *Job, spec *config.JobSpec, runnerName string) map[string]string {
	deadline := time.Now().Add(time.Duration(spec.Timeout+spec.Keep+30) * time.Minute)
	tags := map[string]string{
		"Name":          fmt.Sprintf("truner %s %s", job.FullName(), job.Name),
		TagManaged:      "true",
		TagStack:        cp.Env.Stack,
		TagRole:         "runner",
		TagJobID:        fmt.Sprint(job.JobID),
		TagRunnerName:   runnerName,
		TagRepo:         job.FullName(),
		TagWorkflow:     job.Workflow,
		TagDeadline:     fmt.Sprint(deadline.Unix()),
		TagImage:        spec.Image,
		TagVersion:      cp.Env.Version,
		"truner:run-id": fmt.Sprint(job.RunID),
	}
	for k, v := range spec.Tags {
		if !strings.HasPrefix(strings.ToLower(k), "aws:") && !strings.HasPrefix(k, "truner:") {
			tags[k] = v
		}
	}
	return tags
}

func (cp *ControlPlane) agentConfig(ctx context.Context, job *Job, spec *config.JobSpec, img *ResolvedImage, runnerName, jit string, rel *runnerRelease, fatal string) *agentcfg.Config {
	ac := &agentcfg.Config{
		Version:       cp.Env.Version,
		Mode:          agentcfg.ModeRun,
		Platform:      spec.Platform(),
		Arch:          spec.Arch,
		Stack:         cp.Env.Stack,
		Region:        cp.Env.Region,
		Bucket:        cp.Env.Bucket,
		RunnerName:    runnerName,
		JITConfig:     jit,
		RunnerVersion: rel.Version,
		RunnerURLs:    cp.runnerURLs(ctx, rel, spec.Platform(), spec.Arch),
		Profile:       spec.ImageSpec.Profile,
		ImageName:     spec.Image,
		ImageOS:       spec.ImageSpec.OS,
		User:          spec.ImageSpec.User,
		Env:           spec.Env,
		UseNVMe:       true,
		IdleMinutes:   spec.IdleTimeout,
		MaxMinutes:    spec.Timeout,
		KeepMinutes:   spec.Keep,
		LogKey:        fmt.Sprintf("logs/%s/%s.log", time.Now().UTC().Format("2006-01-02"), runnerName),
		FatalError:    fatal,
		Pool:          spec.Pool,
		Job: agentcfg.JobInfo{
			Repo: job.FullName(), JobID: job.JobID, RunID: job.RunID, Workflow: job.Workflow, Name: job.Name,
			Labels: job.Labels, Spec: spec.Summary(), QueuedAt: job.TS, Warnings: spec.Warnings,
		},
	}
	if img != nil && img.Baked {
		ac.Profile = "baked:" + spec.ImageSpec.Profile
	}
	if spec.Cache == config.CacheS3 {
		ac.Cache = agentcfg.CacheConfig{Enabled: true, Bucket: cp.Env.Bucket, Prefix: cachestore.RepoPrefix(job.Owner, job.Repo), Region: cp.Env.Region}
	}
	if cp.Env.ECRRepoURI != "" {
		ac.ECRRepo = cp.Env.ECRRepoURI
		ac.ECRRegistry = strings.SplitN(cp.Env.ECRRepoURI, "/", 2)[0]
	}
	if spec.SSH && job.Sender != "" {
		ac.SSHUsers = []string{job.Sender}
	}
	return ac
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
