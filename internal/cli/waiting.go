package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/codician-team/truner/internal/controlplane"
	"github.com/codician-team/truner/internal/github"
)

// takenGrace is how long a job whose machine another job took may wait for a
// second machine of the same labels (an EC2 boot) before doctor reports it.
const takenGrace = 5 * time.Minute

// checkWaitingJobs reports queued jobs that no machine is coming for. GitHub
// shows them as queued without a reason, and the rest of the installation
// looks healthy:
//   - labels written for another installation's prefix (`truner/…` sent to a
//     `truner-acme` installation), which the control plane records;
//   - jobs whose machine ran another job with the same labels, when no machine
//     was launched for that other job (its queued event came late).
//
// GitHub confirms each one is still queued, so a job that another
// installation ran, or that was cancelled, is not reported.
func checkWaitingJobs(ctx context.Context, u *ui, e *env, gh *github.Client, jobs []controlplane.Job, problems *int) {
	prefix := e.labelPrefix()
	u.ok("jobs run here when runs-on starts with %s/, for example %s", prefix, exampleLabel(prefix, "cpu=4", "build"))
	queued := func(installationID int64, owner, repo string, id int64) bool {
		gj, err := gh.GetJob(ctx, installationID, owner, repo, id)
		return err == nil && gj.Status == "queued"
	}
	waiting := 0
	items, err := e.store().ItemsInColl(ctx, controlplane.CollForeignPrefix)
	if err != nil {
		u.warn("could not list jobs with other label prefixes: %v", err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Int("ts") > items[j].Int("ts") })
	for _, it := range items[:min(len(items), 5)] {
		if !queued(it.Int("installationId"), it.Str("owner"), it.Str("repo"), it.Int("jobId")) {
			continue
		}
		waiting++
		*problems++
		label, foreign := it.Str("label"), it.Str("prefix")
		u.bad("%s/%s: %s / %s has waited %s: its label %s starts with %s, but this installation's prefix is %s. Use %s",
			it.Str("owner"), it.Str("repo"), it.Str("workflow"), it.Str("name"), dur(time.Since(time.Unix(it.Int("ts"), 0))),
			label, foreign, prefix, relabel(label, foreign, prefix))
	}
	names := map[int64]string{}
	for _, j := range jobs {
		names[j.JobID] = j.Workflow + " / " + j.Name
	}
	taken := machineTaken(jobs, time.Now(), takenGrace)
	for _, j := range taken[:min(len(taken), 5)] {
		if !queued(j.InstallationID, j.Owner, j.Repo, j.JobID) {
			continue
		}
		waiting++
		*problems++
		u.bad("%s: %s / %s has waited %s with no machine: the one launched for it ran %s, which has the same labels. Re-run the job, and give every job its own run= token (run=${{ github.run_id }}-<job>)",
			j.FullName(), j.Workflow, j.Name, dur(time.Since(time.Unix(j.TS, 0))), firstNonEmpty(names[j.PickedBy], fmt.Sprintf("job %d", j.PickedBy)))
	}
	if waiting == 0 {
		u.ok("no queued job is waiting for a machine that is not coming")
	}
}

// machineTaken returns the jobs, newest first, that have not started although
// the machine launched for them ran another job (same labels) more than grace
// ago.
func machineTaken(jobs []controlplane.Job, now time.Time, grace time.Duration) []controlplane.Job {
	var out []controlplane.Job
	for _, j := range jobs {
		if j.Status == controlplane.StatusLaunched && j.PickedBy != 0 && j.PickedBy != j.JobID && j.StartedAt == 0 &&
			j.LaunchedAt > 0 && now.Sub(time.Unix(j.LaunchedAt, 0)) > grace {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].TS > out[b].TS })
	return out
}

// relabel writes label with this installation's prefix in place of foreign.
func relabel(label, foreign, prefix string) string {
	rest, ok := strings.CutPrefix(label, foreign)
	if !ok {
		return prefix
	}
	return prefix + rest
}
