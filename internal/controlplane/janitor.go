package controlplane

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/codician-team/truner/internal/costs"
	"github.com/codician-team/truner/internal/github"
)

// RunJanitor performs all periodic maintenance. Each task is isolated so
// one failure does not stop the others.
func (cp *ControlPlane) RunJanitor(ctx context.Context) error {
	var errs []error
	run := func(name string, timeout time.Duration, fn func(context.Context) error) {
		tctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		start := time.Now()
		if err := fn(tctx); err != nil {
			cp.Log.Error("janitor task failed", "task", name, "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			return
		}
		cp.Log.Debug("janitor task done", "task", name, "took", time.Since(start))
	}
	var instances []ec2types.Instance
	run("instances", 60*time.Second, func(ctx context.Context) error {
		var err error
		instances, err = cp.reconcileInstances(ctx)
		return err
	})
	run("jobs", 90*time.Second, func(ctx context.Context) error { return cp.reconcileJobs(ctx, instances) })
	run("pools", 60*time.Second, func(ctx context.Context) error { return cp.ReconcilePools(ctx, instances) })
	run("bakes", 60*time.Second, cp.ProcessBakes)
	run("launch-templates", 30*time.Second, cp.cleanupLaunchTemplates)
	run("webhook-redelivery", 30*time.Second, cp.redeliverWebhooks)
	run("reruns", 30*time.Second, cp.processReruns)
	if cp.everyN(ctx, "autobake", time.Hour) {
		run("autobake", 60*time.Second, cp.AutoBake)
	}
	run("runner-mirror", 3*time.Minute, func(ctx context.Context) error { return cp.MirrorRunner(ctx, false) })
	if cp.Env.DailyReport {
		run("daily-report", 30*time.Second, cp.dailyReport)
	}
	return errors.Join(errs...)
}

// everyN returns true at most once per interval across all janitor invocations.
func (cp *ControlPlane) everyN(ctx context.Context, name string, every time.Duration) bool {
	slot := time.Now().Unix() / int64(every.Seconds())
	ok, err := cp.Store.PutItem(ctx, fmt.Sprintf("tick#%s#%d", name, slot), Item{}, 2*every, true)
	return err == nil && ok
}

func (cp *ControlPlane) reconcileInstances(ctx context.Context) ([]ec2types.Instance, error) {
	insts, err := cp.ManagedInstances(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	running := 0
	for _, inst := range insts {
		id := aws.ToString(inst.InstanceId)
		state := inst.State.Name
		if state == ec2types.InstanceStateNameShuttingDown || state == ec2types.InstanceStateNameTerminated {
			continue
		}
		role := InstanceTag(inst, TagRole)
		if dl, err := strconv.ParseInt(InstanceTag(inst, TagDeadline), 10, 64); err == nil && dl > 0 && now.Unix() > dl {
			cp.Log.Warn("instance passed its deadline, terminating", "instance", id, "role", role)
			cp.TerminateInstances(ctx, id)
			cp.emit("DeadlineTerminations", 1, UnitCount)
			continue
		}
		if role != "runner" || state != ec2types.InstanceStateNameRunning {
			continue
		}
		running++
		jobID, _ := strconv.ParseInt(InstanceTag(inst, TagJobID), 10, 64)
		if jobID == 0 {
			continue
		}
		launch, err := cp.Store.GetJob(ctx, jobID)
		if err != nil || launch == nil {
			continue
		}
		// The job that ran on this machine is over: the agent normally powers
		// off right away; make sure it does.
		if launch.MachineDone > 0 && launch.InstanceID == id {
			grace := time.Duration(launch.KeepMinutes+10) * time.Minute
			if now.Sub(time.Unix(launch.MachineDone, 0)) > grace {
				cp.Log.Info("terminating machine whose job finished", "instance", id, "job", jobID)
				cp.TerminateInstances(ctx, id)
			}
		}
	}
	cp.emit("ActiveRunners", float64(running), UnitCount)
	return insts, nil
}

// reconcileJobs repairs jobs stuck between webhook and runner pickup: lost
// queue messages, crashed workers and machines that died before starting
// (for example spot interruptions during boot).
func (cp *ControlPlane) reconcileJobs(ctx context.Context, instances []ec2types.Instance) error {
	byID := map[string]ec2types.Instance{}
	for _, i := range instances {
		byID[aws.ToString(i.InstanceId)] = i
	}
	var jobs []Job
	for _, day := range []time.Time{time.Now(), time.Now().Add(-24 * time.Hour)} {
		js, err := cp.Store.JobsInColl(ctx, JobColl(day))
		if err != nil {
			return err
		}
		jobs = append(jobs, js...)
	}
	now := time.Now()
	ghCalls := 0
	var gh *github.Client
	for i := range jobs {
		j := &jobs[i]
		updated := time.Unix(j.UpdatedAt, 0)
		switch j.Status {
		case StatusQueued:
			if now.Sub(updated) > 20*time.Minute {
				cp.Log.Warn("re-enqueueing idle queued job", "job", j.JobID)
				cp.Store.UpdateJob(ctx, j.JobID, map[string]any{"status": StatusQueued}, StatusIn(StatusQueued))
				cp.enqueue(ctx, j.JobID, 0)
			}
		case StatusLaunching:
			if now.Sub(updated) > 10*time.Minute {
				cp.Log.Warn("resetting stuck launch", "job", j.JobID)
				if err := cp.Store.UpdateJob(ctx, j.JobID, map[string]any{"status": StatusQueued}, StatusIn(StatusLaunching)); err == nil {
					cp.enqueue(ctx, j.JobID, 0)
				}
			}
		case StatusLaunched:
			if j.PickedBy != 0 || j.LaunchedAt == 0 {
				continue
			}
			age := now.Sub(time.Unix(j.LaunchedAt, 0))
			inst, alive := byID[j.InstanceID]
			if alive && (inst.State.Name == ec2types.InstanceStateNameShuttingDown || inst.State.Name == ec2types.InstanceStateNameTerminated) {
				alive = false
			}
			idleLimit := 25 * time.Minute
			if (!alive && age > 90*time.Second) || (alive && age > idleLimit) {
				if ghCalls >= 40 {
					continue
				}
				if gh == nil {
					var err error
					if gh, err = cp.GitHub(ctx); err != nil {
						return err
					}
				}
				ghCalls++
				cp.checkUnpickedJob(ctx, gh, j, alive)
			}
		}
	}
	return nil
}

// checkUnpickedJob handles a launched machine whose runner never took a job.
func (cp *ControlPlane) checkUnpickedJob(ctx context.Context, gh *github.Client, j *Job, alive bool) {
	gj, err := gh.GetJob(ctx, j.InstallationID, j.Owner, j.Repo, j.JobID)
	if err != nil {
		cp.Log.Warn("could not check job", "job", j.JobID, "err", err)
		return
	}
	if gj.Status != "queued" {
		// Taken by another runner or finished; only the machine is left over.
		if alive {
			if r, err := gh.GetRunner(ctx, j.InstallationID, j.Owner, j.Repo, j.RunnerID); err != nil || !r.Busy {
				cp.TerminateInstances(ctx, j.InstanceID)
			}
		}
		if gj.Status == "completed" {
			cp.Store.UpdateJob(ctx, j.JobID, map[string]any{"status": StatusCancelled, "conclusion": gj.Conclusion}, StatusIn(StatusLaunched))
		}
		return
	}
	reason := "machine stopped before picking up the job"
	if alive {
		reason = "runner did not pick up the job in time"
		cp.TerminateInstances(ctx, j.InstanceID)
	}
	if j.RunnerID != 0 {
		gh.DeleteRunner(ctx, j.InstallationID, j.Owner, j.Repo, j.RunnerID)
	}
	if j.Attempts+1 >= maxLaunchAttempts {
		cp.Store.UpdateJob(ctx, j.JobID, map[string]any{"status": StatusFailed, "error": reason}, StatusIn(StatusLaunched))
		return
	}
	cp.Log.Warn("relaunching job", "job", j.JobID, "reason", reason, "interrupted", j.Interrupted)
	set := map[string]any{"status": StatusQueued, "attempts": j.Attempts + 1, "error": reason, "instanceId": "", "runnerId": 0}
	if j.Interrupted || j.Lifecycle == "spot" && !alive {
		set["forceOnDemand"] = true
	}
	if err := cp.Store.UpdateJob(ctx, j.JobID, set, StatusIn(StatusLaunched)); err == nil {
		cp.enqueue(ctx, j.JobID, 0)
		cp.emit("Relaunches", 1, UnitCount)
	}
}

func (cp *ControlPlane) cleanupLaunchTemplates(ctx context.Context) error {
	p := ec2.NewDescribeLaunchTemplatesPaginator(cp.EC2, &ec2.DescribeLaunchTemplatesInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag:" + TagStack), Values: []string{cp.Env.Stack}},
	}})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, lt := range page.LaunchTemplates {
			if time.Since(aws.ToTime(lt.CreateTime)) > 30*time.Minute {
				cp.EC2.DeleteLaunchTemplate(ctx, &ec2.DeleteLaunchTemplateInput{LaunchTemplateId: lt.LaunchTemplateId})
			}
		}
	}
	return nil
}

// redeliverWebhooks retries workflow_job deliveries that failed (for example
// during a deploy), so no job is left waiting forever.
func (cp *ControlPlane) redeliverWebhooks(ctx context.Context) error {
	gh, err := cp.GitHub(ctx)
	if err != nil {
		return err
	}
	ds, err := gh.ListHookDeliveries(ctx, 100)
	if err != nil {
		return err
	}
	ok := map[string]bool{}
	latest := map[string]github.HookDelivery{}
	for _, d := range ds {
		if d.StatusCode >= 200 && d.StatusCode < 300 {
			ok[d.GUID] = true
		}
		if cur, seen := latest[d.GUID]; !seen || d.DeliveredAt > cur.DeliveredAt {
			latest[d.GUID] = d
		}
	}
	for guid, d := range latest {
		if ok[guid] || d.Event != "workflow_job" || d.StatusCode == 401 {
			continue
		}
		t, err := time.Parse(time.RFC3339, d.DeliveredAt)
		if err != nil || time.Since(t) > 2*time.Hour {
			continue
		}
		n, err := cp.Store.AddNumber(ctx, "redeliver#"+guid, "n", 1, 24*time.Hour)
		if err != nil || n > 3 {
			continue
		}
		if err := gh.RedeliverHook(ctx, d.ID); err != nil {
			cp.Log.Warn("redelivery failed", "guid", guid, "err", err)
			continue
		}
		cp.Log.Info("redelivered failed webhook", "guid", guid, "action", d.Action, "status", d.StatusCode)
		cp.emit("WebhookRedeliveries", 1, UnitCount)
	}
	return nil
}

func (cp *ControlPlane) processReruns(ctx context.Context) error {
	items, err := cp.Store.ItemsInColl(ctx, collReruns)
	if err != nil || len(items) == 0 {
		return err
	}
	gh, err := cp.GitHub(ctx)
	if err != nil {
		return err
	}
	for _, it := range items {
		pk := it.Str("pk")
		if time.Since(time.Unix(it.Int("ts"), 0)) > 24*time.Hour {
			cp.Store.DeleteItem(ctx, pk)
			continue
		}
		err := gh.RerunJob(ctx, it.Int("installationId"), it.Str("owner"), it.Str("repo"), it.Int("jobId"))
		if err != nil {
			// Usually "the workflow run is still in progress"; try again later.
			cp.Log.Info("re-run not possible yet", "job", it.Int("jobId"), "err", err)
			continue
		}
		cp.Log.Info("re-ran job after spot interruption", "job", it.Int("jobId"))
		cp.Store.DeleteItem(ctx, pk)
	}
	return nil
}

// HandleSpotInterruption marks the job of an interrupted instance so it can
// be retried on on-demand capacity.
func (cp *ControlPlane) HandleSpotInterruption(ctx context.Context, instanceID string) error {
	inst, err := cp.describeInstance(ctx, instanceID)
	if err != nil || inst == nil || InstanceTag(*inst, TagStack) != cp.Env.Stack {
		return err
	}
	cp.emit("SpotInterruptions", 1, UnitCount)
	jobID, _ := strconv.ParseInt(InstanceTag(*inst, TagJobID), 10, 64)
	cp.Log.Warn("spot interruption warning", "instance", instanceID, "job", jobID)
	if jobID == 0 {
		return nil
	}
	return ignoreCond(cp.Store.UpdateJob(ctx, jobID, map[string]any{"interrupted": true}, nil))
}

// HandleInstanceStateChange reacts quickly to machines stopping or dying.
func (cp *ControlPlane) HandleInstanceStateChange(ctx context.Context, instanceID, state string) error {
	inst, err := cp.describeInstance(ctx, instanceID)
	if err != nil || inst == nil || InstanceTag(*inst, TagStack) != cp.Env.Stack {
		return err
	}
	switch InstanceTag(*inst, TagRole) {
	case "builder":
		if state == "stopped" {
			return cp.ProcessBakes(ctx)
		}
	case "pool":
		if state == "stopped" {
			insts, err := cp.ManagedInstances(ctx)
			if err != nil {
				return err
			}
			return cp.ReconcilePools(ctx, insts)
		}
	case "runner":
		if state != "terminated" && state != "shutting-down" {
			return nil
		}
		jobID, _ := strconv.ParseInt(InstanceTag(*inst, TagJobID), 10, 64)
		j, err := cp.Store.GetJob(ctx, jobID)
		if err != nil || j == nil || j.Status != StatusLaunched || j.PickedBy != 0 || j.InstanceID != instanceID {
			return err
		}
		gh, err := cp.GitHub(ctx)
		if err != nil {
			return err
		}
		cp.checkUnpickedJob(ctx, gh, j, false)
	}
	return nil
}

func (cp *ControlPlane) describeInstance(ctx context.Context, id string) (*ec2types.Instance, error) {
	out, err := cp.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		if strings.Contains(err.Error(), "NotFound") {
			return nil, nil
		}
		return nil, err
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			return &i, nil
		}
	}
	return nil, nil
}

// dailyReport emails yesterday's usage summary once a day.
func (cp *ControlPlane) dailyReport(ctx context.Context) error {
	day := time.Now().UTC().Add(-24 * time.Hour)
	if time.Now().UTC().Hour() < 1 {
		return nil // give late webhooks a moment
	}
	ok, err := cp.Store.PutItem(ctx, "report#"+day.Format("2006-01-02"), Item{}, 7*24*time.Hour, true)
	if err != nil || !ok {
		return err
	}
	jobs, err := cp.Store.JobsInColl(ctx, JobColl(day))
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return nil
	}
	sum := costs.Summarize(toUsage(jobs))
	var b strings.Builder
	fmt.Fprintf(&b, "truner usage for %s (stack %s)\n\n", day.Format("Mon 2 Jan 2006"), cp.Env.Stack)
	fmt.Fprintf(&b, "Jobs:            %d (%d failed to launch)\n", sum.Jobs, sum.LaunchFailures)
	fmt.Fprintf(&b, "Runner minutes:  %.0f\n", sum.Minutes)
	fmt.Fprintf(&b, "Spot share:      %.0f%%\n", sum.SpotShare*100)
	fmt.Fprintf(&b, "Estimated cost:  $%.2f\n", sum.Cost)
	fmt.Fprintf(&b, "GitHub-hosted:   ~$%.2f (estimate)\n", sum.GitHubCost)
	if sum.GitHubCost > sum.Cost {
		fmt.Fprintf(&b, "Saved:           ~$%.2f (%.0f%%)\n", sum.GitHubCost-sum.Cost, (1-sum.Cost/sum.GitHubCost)*100)
	}
	fmt.Fprintf(&b, "Median wait:     %.0fs from queued to started\n\nTop repositories:\n", sum.MedianWait)
	type kv struct {
		k string
		v float64
	}
	var repos []kv
	for k, v := range sum.ByRepo {
		repos = append(repos, kv{k, v})
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].v > repos[j].v })
	for i, r := range repos {
		if i == 5 {
			break
		}
		fmt.Fprintf(&b, "  %-40s $%.2f\n", r.k, r.v)
	}
	cp.alert(ctx, fmt.Sprintf("truner daily report: %d jobs, $%.2f", sum.Jobs, sum.Cost), b.String())
	return nil
}

func toUsage(jobs []Job) []costs.Usage {
	out := make([]costs.Usage, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, costs.Usage{
			Repo: j.FullName(), Workflow: j.Workflow, Status: j.Status, Cost: j.Cost, CPU: j.CPU, Platform: j.Platform,
			InstanceType: firstNonEmpty(j.RanOnType, j.InstanceType), Lifecycle: firstNonEmpty(j.RanOnLifecycle, j.Lifecycle),
			QueuedAt: j.TS, StartedAt: j.StartedAt, CompletedAt: j.CompletedAt,
		})
	}
	return out
}
