package controlplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	mvmtypes "github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"

	"github.com/codician-team/truner/internal/github"
)

// Jobs with equal labels run on each other's machines: GitHub gives a job to
// whichever matching runner is idle first. That is harmless while every job
// gets a machine. When a job starts on another job's machine and none was
// launched for itself (its queued event came late, or its launch was
// deferred), one job with those labels is left without a machine, and GitHub
// keeps it queued.
//
// The control plane launches a machine for that job again, on its own record,
// so the worker, the janitor and the webhooks handle it like any launch. The
// machine it had is still running the other job: it is kept apart under its
// runner name first, where that job's completion finds it to book its cost
// and end it.

// takenCheckDelay lets the in_progress events of the other jobs arrive before
// the control plane looks for a job left without a machine.
const takenCheckDelay = 30 * time.Second

// keptMachineTTL is how long a machine kept apart from its job is remembered.
const keptMachineTTL = 7 * 24 * time.Hour

// noteTaken is called when a job started on a machine launched for another job
// while none was launched for itself. It asks for a check of the repository's
// jobs.
func (cp *ControlPlane) noteTaken(ctx context.Context, jobID int64, repo, runnerName string) {
	lid := launchJobID(runnerName)
	if lid == 0 || lid == jobID {
		return
	}
	cp.Log.Warn("job started on another job's machine before one was launched for it: checking that every job with the same labels gets a machine",
		"job", jobID, "repo", repo, "ranOn", runnerName, "launchedFor", lid)
	cp.emit("MachinesTaken", 1, UnitCount)
	if err := cp.send(ctx, queueMessage{JobID: jobID, Taken: true}, takenCheckDelay); err != nil {
		cp.Log.Warn("could not queue the check; the janitor does it", "job", jobID, "err", err)
	}
}

// checkTaken looks for jobs left without a machine in the repository of a job
// that started on another job's machine.
func (cp *ControlPlane) checkTaken(ctx context.Context, jobID int64) error {
	job, err := cp.Store.GetJob(ctx, jobID)
	if err != nil || job == nil {
		return err
	}
	all, err := cp.recentJobs(ctx)
	if err != nil {
		return err
	}
	var jobs []Job
	for _, j := range all {
		if strings.EqualFold(j.FullName(), job.FullName()) {
			jobs = append(jobs, j)
		}
	}
	insts, err := cp.ManagedInstances(ctx, "pending", "running")
	if err != nil {
		return err
	}
	// No MicroVM list: this runs after every swap, and ListMicrovms is rate
	// limited. A job that waits for a MicroVM that died gets a machine from the
	// janitor, which lists them.
	waiting := withoutMachine(jobs, machinesUp(insts, nil), time.Now())
	if len(waiting) == 0 {
		return nil
	}
	gh, err := cp.GitHub(ctx)
	if err != nil {
		return err
	}
	budget := 10
	return cp.relaunchTaken(ctx, gh, waiting, &budget)
}

// withoutMachine returns the jobs, oldest first, whose machine ran another job
// and that no spare machine can run. A spare machine is one whose own job
// started elsewhere (the other half of a swap) and that is still booting or
// idle: a waiting job takes it.
func withoutMachine(jobs []Job, up func(id string) bool, now time.Time) []*Job {
	var waiting, spare []*Job
	for i := range jobs {
		j := &jobs[i]
		if j.Status == StatusLaunched && j.StartedAt == 0 && j.PickedBy != 0 && j.PickedBy != j.JobID {
			if now.Sub(time.Unix(j.TS, 0)) < 24*time.Hour { // GitHub gives up after a day
				waiting = append(waiting, j)
			}
			continue
		}
		started := j.StartedAt > 0 || j.Status == StatusRunning || j.Status == StatusCompleted
		if started && j.InstanceID != "" && j.PickedBy == 0 && j.MachineDone == 0 && j.LaunchedAt > 0 &&
			now.Sub(time.Unix(j.LaunchedAt, 0)) < idleLimit(j.InstanceID) && up(j.InstanceID) {
			spare = append(spare, j)
		}
	}
	sort.Slice(waiting, func(a, b int) bool { return waiting[a].TS < waiting[b].TS })
	var out []*Job
	for _, w := range waiting {
		i := slices.IndexFunc(spare, func(s *Job) bool {
			return s != nil && strings.EqualFold(s.FullName(), w.FullName()) && covers(s.Labels, w.Labels)
		})
		if i >= 0 {
			spare[i] = nil // w takes it
			continue
		}
		out = append(out, w)
	}
	return out
}

// covers reports whether a runner registered with labels have can run a job
// that asks for want. GitHub compares labels case-insensitively.
func covers(have, want []string) bool {
	for _, w := range want {
		if !slices.ContainsFunc(have, func(h string) bool { return strings.EqualFold(h, w) }) {
			return false
		}
	}
	return true
}

// idleLimit is how long a machine may wait for a job before the janitor takes
// it for broken.
func idleLimit(instanceID string) time.Duration {
	if isMicroVM(instanceID) {
		return 5 * time.Minute // its runner starts within seconds
	}
	return 25 * time.Minute
}

// machinesUp reports whether a machine is up: an EC2 instance that is pending
// or running, or a MicroVM that is not terminating. With no MicroVM list
// (it failed), every MicroVM counts as up.
func machinesUp(instances []ec2types.Instance, microvms map[string]mvmtypes.MicrovmItem) func(string) bool {
	up := map[string]bool{}
	for _, i := range instances {
		if i.State != nil && (i.State.Name == ec2types.InstanceStateNamePending || i.State.Name == ec2types.InstanceStateNameRunning) {
			up[aws.ToString(i.InstanceId)] = true
		}
	}
	return func(id string) bool {
		if isMicroVM(id) {
			if microvms == nil {
				return true
			}
			_, ok := microvms[id]
			return ok
		}
		return up[id]
	}
}

// relaunchTaken launches a machine for each waiting job that GitHub still has
// queued. budget bounds the GitHub calls.
func (cp *ControlPlane) relaunchTaken(ctx context.Context, gh *github.Client, waiting []*Job, budget *int) error {
	var errs []error
	for _, w := range waiting {
		if *budget <= 0 {
			break
		}
		*budget--
		gj, err := gh.GetJob(ctx, w.InstallationID, w.Owner, w.Repo, w.JobID)
		if err != nil {
			cp.Log.Warn("could not check job", "job", w.JobID, "err", err)
			continue
		}
		switch gj.Status {
		case "queued":
			errs = append(errs, cp.relaunchOne(ctx, w.JobID))
		case "completed":
			// Cancelled while it waited, and the webhook was lost.
			cp.Store.UpdateJob(ctx, w.JobID, map[string]any{"status": StatusCancelled, "conclusion": gj.Conclusion}, StatusIn(StatusLaunched))
		}
	}
	return errors.Join(errs...)
}

// relaunchOne gives a job whose machine runs another job a machine of its
// own. The taken machine is kept under its runner name first.
func (cp *ControlPlane) relaunchOne(ctx context.Context, jobID int64) error {
	w, err := cp.Store.GetJob(ctx, jobID)
	if err != nil || w == nil {
		return err
	}
	if w.Status != StatusLaunched || w.StartedAt != 0 || w.PickedBy == 0 || w.PickedBy == w.JobID || w.RunnerName == "" {
		return nil // it started or changed meanwhile
	}
	now := time.Now()
	kept := *w
	kept.PK, kept.Coll, kept.TS, kept.TTL = MachinePK(w.RunnerName), collMachines, now.Unix(), now.Add(keptMachineTTL).Unix()
	if err := cp.Store.PutJob(ctx, &kept); err != nil {
		return err
	}
	set := map[string]any{
		"status": StatusQueued, "error": fmt.Sprintf("the machine launched for it ran job %d", w.PickedBy),
		"runnerName": "", "runnerId": 0, "instanceId": "", "instanceType": "", "lifecycle": "", "az": "", "ami": "", "pool": "",
		"price": 0, "launchedAt": 0, "launchMillis": 0, "pickedBy": 0, "machineDoneAt": 0, "interrupted": false, "fellBack": false,
	}
	// Only if nothing changed since it was kept: the job on the taken machine
	// may complete right now and mark it done.
	done := expression.Name("machineDoneAt").Equal(expression.Value(w.MachineDone))
	if w.MachineDone == 0 {
		done = expression.AttributeNotExists(expression.Name("machineDoneAt")).Or(done)
	}
	cond := expression.Name("status").Equal(expression.Value(StatusLaunched)).
		And(expression.Name("runnerName").Equal(expression.Value(w.RunnerName))).
		And(expression.Name("pickedBy").Equal(expression.Value(w.PickedBy))).
		And(done)
	err = cp.Store.UpdateJob(ctx, jobID, set, &cond)
	if errors.Is(err, ErrConditionFailed) {
		return nil // the next check looks again
	}
	if err != nil {
		return err
	}
	cp.Log.Warn("launching a machine again for a job whose machine ran another job", "job", jobID, "repo", w.FullName(),
		"machineRan", w.PickedBy, "takenRunner", w.RunnerName, "takenMachine", w.InstanceID)
	cp.emit("TakenRelaunches", 1, UnitCount)
	return cp.enqueue(ctx, jobID, 0)
}

// machineOf returns the record of the machine a runner ran on: the record of
// the job it was launched for (rec, when that is the one), or the machine kept
// apart when that job was launched again (relaunchOne). kept says which.
func (cp *ControlPlane) machineOf(ctx context.Context, runnerName string, rec *Job) (m *Job, kept bool, err error) {
	lid := launchJobID(runnerName)
	if rec == nil || rec.JobID != lid {
		if rec, err = cp.Store.GetJob(ctx, lid); err != nil {
			return nil, false, err
		}
	}
	if rec != nil && rec.RunnerName == runnerName {
		return rec, false, nil
	}
	m, err = cp.Store.GetMachine(ctx, runnerName)
	if err != nil || m == nil {
		return rec, false, err
	}
	return m, true, nil
}

// setMachine sets fields of the machine a runner ran on, on the record of the
// job it was launched for while that still holds it, or on the kept machine.
func (cp *ControlPlane) setMachine(ctx context.Context, runnerName string, set map[string]any) error {
	cond := expression.Name("runnerName").Equal(expression.Value(runnerName))
	err := cp.Store.UpdateJob(ctx, launchJobID(runnerName), set, &cond)
	if !errors.Is(err, ErrConditionFailed) {
		return err
	}
	m, err := cp.Store.GetMachine(ctx, runnerName)
	if err != nil || m == nil {
		return err
	}
	return cp.Store.UpdateItem(ctx, MachinePK(runnerName), Item(set))
}
