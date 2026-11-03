package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/codician-team/truner/internal/config"
	"github.com/codician-team/truner/internal/github"
	"github.com/codician-team/truner/internal/labels"
)

// jobTTL is how long job records are kept (for `truner jobs` and cost reports).
const jobTTL = 90 * 24 * time.Hour

// WebhookResponse is the HTTP response for a webhook delivery.
type WebhookResponse struct {
	Status int
	Body   string
}

// HandleWebhook processes one GitHub webhook delivery. headers must have
// lower-case keys.
func (cp *ControlPlane) HandleWebhook(ctx context.Context, headers map[string]string, body []byte) WebhookResponse {
	gh, err := cp.GitHub(ctx)
	if err != nil {
		cp.Log.Error("webhook: no app", "err", err)
		return WebhookResponse{503, "truner is not configured"}
	}
	if !github.VerifySignature(gh.App().WebhookSecret, body, headers["x-hub-signature-256"]) {
		cp.Log.Warn("webhook: bad signature", "delivery", headers["x-github-delivery"])
		return WebhookResponse{401, "bad signature"}
	}
	event := headers["x-github-event"]
	switch event {
	case "ping":
		return WebhookResponse{200, "pong"}
	case "workflow_job":
	default:
		return WebhookResponse{200, "ignored event " + event}
	}
	var ev github.WorkflowJobEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return WebhookResponse{400, "invalid payload"}
	}
	if !labels.Match(cp.Env.LabelPrefix, ev.WorkflowJob.Labels) {
		if label, foreign := labels.ForeignPrefix(cp.Env.LabelPrefix, ev.WorkflowJob.Labels); foreign != "" {
			if ev.Action == "queued" {
				cp.recordForeignPrefix(ctx, &ev, label, foreign)
			}
			return WebhookResponse{200, fmt.Sprintf("not a job for this installation: its label starts with %q, this installation's prefix is %q", foreign, cp.Env.LabelPrefix)}
		}
		return WebhookResponse{200, "not a truner job"}
	}
	log := cp.Log.With("job", ev.WorkflowJob.ID, "repo", ev.Repository.FullName, "action", ev.Action)
	switch ev.Action {
	case "queued":
		err = cp.onQueued(ctx, &ev)
	case "in_progress":
		err = cp.onInProgress(ctx, &ev)
	case "completed":
		err = cp.onCompleted(ctx, gh, &ev)
	default:
		return WebhookResponse{200, "ignored action " + ev.Action}
	}
	if err != nil {
		log.Error("webhook handling failed", "err", err)
		// A 5xx makes the delivery show as failed, so the janitor redelivers it.
		return WebhookResponse{500, "error: " + err.Error()}
	}
	log.Info("webhook handled")
	return WebhookResponse{202, "ok"}
}

func (cp *ControlPlane) onQueued(ctx context.Context, ev *github.WorkflowJobEvent) error {
	wj := ev.WorkflowJob
	now := time.Now()
	queuedAt := now
	if wj.CreatedAt != nil && !wj.CreatedAt.IsZero() && wj.CreatedAt.Before(now) {
		queuedAt = *wj.CreatedAt
	}
	j := &Job{
		PK: JobPK(wj.ID), Coll: JobColl(queuedAt), TS: queuedAt.Unix(),
		JobID: wj.ID, RunID: wj.RunID, RunAttempt: wj.RunAttempt, InstallationID: ev.Installation.ID,
		Owner: ev.Repository.Owner.Login, Repo: ev.Repository.Name, Name: wj.Name, Workflow: wj.WorkflowName,
		Branch: wj.HeadBranch, SHA: wj.HeadSHA, Sender: ev.Sender.Login, Labels: wj.Labels, Private: ev.Repository.Private,
		Status: StatusQueued, UpdatedAt: now.Unix(), TTL: now.Add(jobTTL).Unix(),
	}
	created, err := cp.Store.PutJobIfAbsent(ctx, j)
	if err != nil {
		return err
	}
	if !created {
		return nil // duplicate delivery
	}
	cp.emit("JobsQueued", 1, UnitCount)
	return cp.enqueue(ctx, wj.ID, 0)
}

// recordForeignPrefix notes a job that is meant for truner but carries another
// installation's label prefix (`truner/…` sent to `truner-acme`). Nothing
// starts a machine for it and GitHub only shows it as queued, so it is logged
// and kept for `truner doctor`.
func (cp *ControlPlane) recordForeignPrefix(ctx context.Context, ev *github.WorkflowJobEvent, label, foreign string) {
	wj := ev.WorkflowJob
	cp.Log.Warn("job labels use another label prefix: no machine is started for it",
		"job", wj.ID, "repo", ev.Repository.FullName, "label", label, "prefix", foreign, "installationPrefix", cp.Env.LabelPrefix)
	_, err := cp.Store.PutItem(ctx, fmt.Sprintf("foreignprefix#%d", wj.ID), Item{
		"coll": CollForeignPrefix, "ts": time.Now().Unix(), "jobId": wj.ID, "installationId": ev.Installation.ID,
		"owner": ev.Repository.Owner.Login, "repo": ev.Repository.Name, "workflow": wj.WorkflowName, "name": wj.Name,
		"label": label, "prefix": foreign,
	}, 2*24*time.Hour, true)
	if err != nil {
		cp.Log.Warn("could not record job with another label prefix", "job", wj.ID, "err", err)
	}
}

// queueMessage is the body of a worker queue message: launch a machine for a
// job, or with Taken, check its repository for jobs left without a machine.
type queueMessage struct {
	JobID int64 `json:"jobId"`
	Taken bool  `json:"taken,omitempty"`
}

func (cp *ControlPlane) enqueue(ctx context.Context, jobID int64, delay time.Duration) error {
	return cp.send(ctx, queueMessage{JobID: jobID}, delay)
}

func (cp *ControlPlane) send(ctx context.Context, m queueMessage, delay time.Duration) error {
	body, _ := json.Marshal(m)
	secs := int32(delay.Seconds())
	if secs > 900 {
		secs = 900
	}
	_, err := cp.SQS.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: &cp.Env.QueueURL, MessageBody: aws.String(string(body)), DelaySeconds: secs})
	return err
}

// launchJobID extracts the id of the job a runner was launched for from its
// name (truner-<jobid>-<suffix>).
func launchJobID(runnerName string) int64 {
	if !strings.HasPrefix(runnerName, "truner-") {
		return 0
	}
	parts := strings.Split(runnerName, "-")
	if len(parts) < 3 {
		return 0
	}
	id, _ := strconv.ParseInt(parts[1], 10, 64)
	return id
}

func (cp *ControlPlane) onInProgress(ctx context.Context, ev *github.WorkflowJobEvent) error {
	wj := ev.WorkflowJob
	started := time.Now()
	if wj.StartedAt != nil && !wj.StartedAt.IsZero() {
		started = *wj.StartedAt
	}
	set := map[string]any{"status": StatusRunning, "startedAt": started.Unix(), "ranOn": wj.RunnerName}
	err := cp.Store.UpdateJob(ctx, wj.ID, set, nil)
	if err != nil && !errors.Is(err, ErrConditionFailed) {
		return err
	}
	if errors.Is(err, ErrConditionFailed) {
		// We never saw the queued event (it predates the install, or GitHub
		// delivers it late); record it now. No machine is launched for it.
		cp.noteTaken(ctx, wj.ID, ev.Repository.FullName, wj.RunnerName)
		j := &Job{PK: JobPK(wj.ID), Coll: JobColl(started), TS: started.Unix(), JobID: wj.ID, RunID: wj.RunID, RunAttempt: wj.RunAttempt,
			InstallationID: ev.Installation.ID, Owner: ev.Repository.Owner.Login, Repo: ev.Repository.Name, Name: wj.Name,
			Workflow: wj.WorkflowName, Labels: wj.Labels, Status: StatusRunning, StartedAt: started.Unix(), RanOn: wj.RunnerName,
			UpdatedAt: time.Now().Unix(), TTL: time.Now().Add(jobTTL).Unix()}
		if _, err := cp.Store.PutJobIfAbsent(ctx, j); err != nil {
			return err
		}
	}
	if lid := launchJobID(wj.RunnerName); lid != 0 {
		// Only while the record still holds this machine: a job launched
		// again has another one.
		cond := expression.Name("runnerName").Equal(expression.Value(wj.RunnerName))
		err := cp.Store.UpdateJob(ctx, lid, map[string]any{"pickedBy": wj.ID}, &cond)
		if err != nil && !errors.Is(err, ErrConditionFailed) {
			return err
		}
	}
	if job, _ := cp.Store.GetJob(ctx, wj.ID); job != nil {
		cp.emit("QueueToStartSeconds", float64(started.Unix()-job.TS), UnitSeconds)
	}
	return nil
}

func (cp *ControlPlane) onCompleted(ctx context.Context, gh *github.Client, ev *github.WorkflowJobEvent) error {
	wj := ev.WorkflowJob
	completed := time.Now()
	if wj.CompletedAt != nil && !wj.CompletedAt.IsZero() {
		completed = *wj.CompletedAt
	}
	job, err := cp.Store.GetJob(ctx, wj.ID)
	if err != nil {
		return err
	}
	set := map[string]any{"completedAt": completed.Unix(), "conclusion": wj.Conclusion}

	lid := launchJobID(wj.RunnerName)
	if lid == 0 {
		// Never picked up by a truner runner (cancelled while queued). Stop
		// the machine launched for it unless its runner is already busy with
		// another job carrying the same labels.
		if job == nil || (job.Status != StatusFailed && job.Status != StatusRejected) {
			set["status"] = StatusCancelled
		}
		if job != nil && job.InstanceID != "" && job.PickedBy == 0 {
			busy := false
			if job.RunnerID != 0 {
				if r, err := gh.GetRunner(ctx, job.InstallationID, job.Owner, job.Repo, job.RunnerID); err == nil {
					busy = r.Busy
				}
			}
			if !busy {
				cp.Log.Info("job ended before pickup, terminating its machine", "job", wj.ID, "instance", job.InstanceID)
				if err := cp.TerminateInstances(ctx, job.InstanceID); err != nil {
					cp.Log.Warn("terminate failed", "err", err)
				}
				if job.RunnerID != 0 {
					gh.DeleteRunner(ctx, job.InstallationID, job.Owner, job.Repo, job.RunnerID)
				}
				set["machineDoneAt"] = completed.Unix()
				if job.LaunchedAt > 0 {
					cost := job.machineCost(completed)
					set["cost"] = cost
					cp.addSpend(ctx, cost)
				}
			}
		}
		if job == nil {
			return nil
		}
		return ignoreCond(cp.Store.UpdateJob(ctx, wj.ID, set, nil))
	}

	set["status"] = StatusCompleted
	set["ranOn"] = wj.RunnerName
	launch, kept, err := cp.machineOf(ctx, wj.RunnerName, job)
	if err != nil {
		return err
	}
	if launch != nil {
		set["ranOnInstance"] = launch.InstanceID
		set["ranOnType"] = launch.InstanceType
		set["ranOnLifecycle"] = launch.Lifecycle
		if launch.LaunchedAt > 0 {
			cost := launch.machineCost(completed)
			set["cost"] = cost
			cp.addSpend(ctx, cost)
			cp.emit("JobCostUSD", cost, UnitNone)
		}
		if wj.StartedAt != nil && !wj.StartedAt.IsZero() {
			cp.emit("JobDurationSeconds", completed.Sub(*wj.StartedAt).Seconds(), UnitSeconds)
		}
		// The machine's job is over. Only machine fields of the launch record
		// change: with identical labels (e.g. matrix jobs) the job the machine
		// was launched for may still be running elsewhere.
		if lid != wj.ID || kept {
			if err := cp.setMachine(ctx, wj.RunnerName, map[string]any{"machineDoneAt": completed.Unix()}); err != nil {
				cp.Log.Warn("could not mark the machine done", "runner", wj.RunnerName, "err", err)
			}
		} else {
			set["machineDoneAt"] = completed.Unix()
		}
		if isMicroVM(launch.InstanceID) {
			// EC2 machines power themselves off; MicroVMs are ended from here.
			if err := cp.TerminateInstances(ctx, launch.InstanceID); err != nil {
				cp.Log.Warn("terminate MicroVM failed", "microvm", launch.InstanceID, "err", err)
			}
		}
		if launch.Interrupted && wj.Conclusion == "failure" {
			cp.scheduleRerun(ctx, ev)
		}
	}
	if job == nil {
		started := completed
		if wj.StartedAt != nil {
			started = *wj.StartedAt
		}
		j := &Job{PK: JobPK(wj.ID), Coll: JobColl(started), TS: started.Unix(), JobID: wj.ID, RunID: wj.RunID, RunAttempt: wj.RunAttempt,
			InstallationID: ev.Installation.ID, Owner: ev.Repository.Owner.Login, Repo: ev.Repository.Name, Name: wj.Name,
			Workflow: wj.WorkflowName, Labels: wj.Labels, Status: StatusCompleted, StartedAt: started.Unix(),
			UpdatedAt: time.Now().Unix(), TTL: time.Now().Add(jobTTL).Unix()}
		if _, err := cp.Store.PutJobIfAbsent(ctx, j); err != nil {
			return err
		}
	}
	cp.emit("JobsCompleted", 1, UnitCount)
	return ignoreCond(cp.Store.UpdateJob(ctx, wj.ID, set, nil))
}

func ignoreCond(err error) error {
	if errors.Is(err, ErrConditionFailed) {
		return nil
	}
	return err
}

// addSpend accumulates the month's estimated spend and raises budget alerts.
func (cp *ControlPlane) addSpend(ctx context.Context, cost float64) {
	if cost <= 0 {
		return
	}
	month := time.Now().UTC().Format("2006-01")
	total, err := cp.Store.AddNumber(ctx, "spend#"+month, "total", cost, 400*24*time.Hour)
	if err != nil {
		cp.Log.Warn("spend counter update failed", "err", err)
		return
	}
	budget := cp.GlobalConfig(ctx).Limits.MonthlyBudgetUSD
	if budget <= 0 {
		return
	}
	for _, pct := range []int{80, 100} {
		if total >= budget*float64(pct)/100 {
			if ok, _ := cp.Store.PutItem(ctx, fmt.Sprintf("alert#%s#%d", month, pct), Item{"at": time.Now().Unix()}, 60*24*time.Hour, true); ok {
				msg := fmt.Sprintf("truner (%s) has spent an estimated $%.2f of its $%.2f monthly budget (%d%%).", cp.Env.Stack, total, budget, pct)
				if pct >= 100 {
					msg += "\nNew jobs will wait in the queue until the budget is raised (`truner config edit`) or the month ends."
				}
				cp.alert(ctx, fmt.Sprintf("truner budget %d%% reached", pct), msg)
			}
		}
	}
}

// MonthSpend returns the estimated spend of the current month.
func (cp *ControlPlane) MonthSpend(ctx context.Context) float64 {
	it, err := cp.Store.GetItem(ctx, "spend#"+time.Now().UTC().Format("2006-01"))
	if err != nil || it == nil {
		return 0
	}
	return it.Float("total")
}

// scheduleRerun records a job killed by a spot interruption so the janitor
// re-runs it (on on-demand capacity) once the workflow run has finished.
func (cp *ControlPlane) scheduleRerun(ctx context.Context, ev *github.WorkflowJobEvent) {
	if !config.BoolOr(cp.GlobalConfig(ctx).RetryOnInterruption, true) {
		return
	}
	if ev.WorkflowJob.RunAttempt >= 3 {
		return
	}
	wj := ev.WorkflowJob
	cp.Store.PutItem(ctx, fmt.Sprintf("rerun#%d", wj.ID), Item{
		"coll": collReruns, "ts": time.Now().Unix(), "jobId": wj.ID, "runId": wj.RunID,
		"owner": ev.Repository.Owner.Login, "repo": ev.Repository.Name, "installationId": ev.Installation.ID,
	}, 2*24*time.Hour, true)
	// Make the retry land on on-demand capacity.
	cp.Store.PutItem(ctx, fmt.Sprintf("ondemand#%s#%d", ev.Repository.FullName, wj.RunID), Item{"at": time.Now().Unix()}, 2*24*time.Hour, false)
	cp.Log.Info("scheduled re-run after spot interruption", "job", wj.ID)
	cp.emit("InterruptionReruns", 1, UnitCount)
}
