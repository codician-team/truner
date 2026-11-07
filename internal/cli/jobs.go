package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"

	"github.com/codician-team/truner/internal/controlplane"
	"github.com/codician-team/truner/internal/costs"
)

func (e *env) jobsSince(ctx context.Context, days int) ([]controlplane.Job, error) {
	var out []controlplane.Job
	for d := 0; d < days; d++ {
		js, err := e.store().JobsInColl(ctx, controlplane.JobColl(time.Now().Add(-time.Duration(d)*24*time.Hour)))
		if err != nil {
			return nil, err
		}
		out = append(out, js...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS > out[j].TS })
	return out, nil
}

func (a *app) jobsCmd() *cobra.Command {
	var repo string
	var limit, days int
	var watch bool
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "List recent jobs with their machine, wait time and cost",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			e, err := a.connect(ctx)
			if err != nil {
				return err
			}
			for {
				jobs, err := e.jobsSince(ctx, days)
				if err != nil {
					return err
				}
				var rows [][]string
				for _, j := range jobs {
					if repo != "" && !strings.EqualFold(j.FullName(), repo) {
						continue
					}
					if len(rows) >= limit {
						break
					}
					machine := firstNonEmpty(j.RanOnType, j.InstanceType)
					if lc := firstNonEmpty(j.RanOnLifecycle, j.Lifecycle); lc != "" && machine != "" {
						machine += " " + strings.Replace(lc, "on-demand", "od", 1)
					}
					if j.Pool != "" {
						machine += " (pool)"
					}
					wait, run := time.Duration(0), time.Duration(0)
					if j.StartedAt > 0 {
						wait = time.Duration(j.StartedAt-j.TS) * time.Second
						if j.CompletedAt > j.StartedAt {
							run = time.Duration(j.CompletedAt-j.StartedAt) * time.Second
						} else if j.Status == controlplane.StatusRunning {
							run = time.Since(time.Unix(j.StartedAt, 0))
						}
					}
					status := j.Status
					if j.Conclusion != "" && j.Status == controlplane.StatusCompleted {
						status = j.Conclusion
					}
					switch status {
					case "success":
						status = a.ui.green(status)
					case "failure", controlplane.StatusFailed:
						status = a.ui.red(status)
					case controlplane.StatusRunning, controlplane.StatusLaunched, controlplane.StatusLaunching, controlplane.StatusQueued:
						status = a.ui.cyan(status)
					}
					name := shorten(j.Workflow+" / "+j.Name, 40)
					rows = append(rows, []string{ago(time.Unix(j.TS, 0)), j.FullName(), name, status, firstNonEmpty(machine, "-"), dur(wait), dur(run), money(j.Cost), strconv.FormatInt(j.JobID, 10)})
				}
				if watch {
					fmt.Fprint(a.ui.out, "\x1b[H\x1b[2J")
				}
				if len(rows) == 0 {
					a.ui.printf("No jobs yet. Use `runs-on: %s` in a workflow, with each job's own name in its run= token.\n", exampleLabel(e.labelPrefix(), "cpu=2", "build"))
				} else {
					a.ui.table([]string{"QUEUED", "REPOSITORY", "WORKFLOW / JOB", "STATUS", "MACHINE", "WAIT", "DURATION", "COST", "JOB ID"}, rows)
				}
				if !watch {
					return nil
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(5 * time.Second):
				}
			}
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "only this repository (owner/name)")
	cmd.Flags().IntVarP(&limit, "limit", "n", 30, "number of jobs")
	cmd.Flags().IntVar(&days, "days", 2, "look back this many days")
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "refresh every 5 seconds")
	return cmd
}

func (a *app) costsCmd() *cobra.Command {
	var days int
	var by string
	cmd := &cobra.Command{
		Use:   "costs",
		Short: "Show estimated spend and savings compared to GitHub-hosted runners",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			u := a.ui
			e, err := a.connect(ctx)
			if err != nil {
				return err
			}
			jobs, err := e.jobsSince(ctx, days)
			if err != nil {
				return err
			}
			var us []costs.Usage
			for _, j := range jobs {
				us = append(us, costs.Usage{Repo: j.FullName(), Workflow: j.Workflow, Status: j.Status, InstanceType: firstNonEmpty(j.RanOnType, j.InstanceType),
					Lifecycle: firstNonEmpty(j.RanOnLifecycle, j.Lifecycle), Platform: j.Platform, CPU: j.CPU, Cost: j.Cost, QueuedAt: j.TS, StartedAt: j.StartedAt, CompletedAt: j.CompletedAt})
			}
			s := costs.Summarize(us)
			u.printf("%s (last %d days)\n\n", u.bold("truner costs"), days)
			u.printf("  Jobs              %d\n", s.Jobs)
			u.printf("  Runner minutes    %.0f\n", s.Minutes)
			u.printf("  Spot share        %.0f%%\n", s.SpotShare*100)
			u.printf("  Estimated cost    %s\n", u.bold(money(s.Cost)))
			u.printf("  GitHub-hosted     ~%s for the same minutes on same-size runners\n", money(s.GitHubCost))
			if s.GitHubCost > s.Cost && s.GitHubCost > 0 {
				u.printf("  You saved         %s\n", u.green(fmt.Sprintf("~%s (%.0f%%)", money(s.GitHubCost-s.Cost), (1-s.Cost/s.GitHubCost)*100)))
			}
			u.printf("  Median wait       %.0fs from queued to started\n\n", s.MedianWait)
			var m map[string]float64
			switch by {
			case "workflow":
				m = s.ByWorkflow
			case "type":
				m = s.ByType
			default:
				m = s.ByRepo
			}
			type kv struct {
				k string
				v float64
			}
			var rows []kv
			for k, v := range m {
				rows = append(rows, kv{k, v})
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].v > rows[j].v })
			var table [][]string
			for i, r := range rows {
				if i == 20 {
					break
				}
				share := 0.0
				if s.Cost > 0 {
					share = r.v / s.Cost * 100
				}
				table = append(table, []string{r.k, money(r.v), fmt.Sprintf("%.0f%%", share)})
			}
			if len(table) > 0 {
				u.table([]string{strings.ToUpper(firstNonEmpty(by, "repo")), "COST", "SHARE"}, table)
			}
			u.printf("\n%s\n", u.dim("Estimates use EC2 spot/on-demand prices at launch plus EBS; GitHub prices are approximate list prices. AWS Cost Explorer has the exact numbers (filter by tag truner:repository)."))
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 30, "look back this many days")
	cmd.Flags().StringVar(&by, "by", "repo", "group by repo, workflow or type")
	return cmd
}

func (a *app) logsCmd() *cobra.Command {
	var controlPlane, follow bool
	var since time.Duration
	var filter string
	cmd := &cobra.Command{
		Use:   "logs [JOB-ID | RUNNER-NAME]",
		Short: "Show a runner machine's boot and agent log, or the control plane logs",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			e, err := a.connect(ctx)
			if err != nil {
				return err
			}
			if controlPlane || len(args) == 0 {
				if len(args) == 1 {
					filter = args[0]
				}
				return controlPlaneLogs(ctx, e, since, filter, follow, a.ui.out)
			}
			key, err := machineLogKey(ctx, e, args[0])
			if err != nil {
				return err
			}
			get := func(ctx context.Context, key string) (io.ReadCloser, error) {
				out, err := e.s3().GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(e.out("DataBucket")), Key: aws.String(key)})
				if err != nil {
					return nil, err
				}
				return out.Body, nil
			}
			return printMachineLog(ctx, get, e.out("DataBucket"), key, os.Stdout)
		},
	}
	cmd.Flags().BoolVar(&controlPlane, "control-plane", false, "show the webhook/worker/janitor logs")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new control plane logs")
	cmd.Flags().DurationVar(&since, "since", time.Hour, "how far back to read control plane logs")
	return cmd
}

func machineLogKey(ctx context.Context, e *env, ref string) (string, error) {
	runner := ref
	launched := time.Now()
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		j, err := e.store().GetJob(ctx, id)
		if err != nil {
			return "", err
		}
		if j == nil {
			return "", fmt.Errorf("job %d not found", id)
		}
		runner = j.RunnerName
		if j.RanOn != "" && j.RanOn != j.RunnerName {
			runner = j.RanOn
		}
		if runner == "" {
			return "", fmt.Errorf("job %d never got a machine (status %s: %s)", id, j.Status, j.Error)
		}
		lid := j.JobID
		if parts := strings.Split(runner, "-"); len(parts) >= 3 {
			lid, _ = strconv.ParseInt(parts[1], 10, 64)
		}
		if lj, err := e.store().GetJob(ctx, lid); err == nil && lj != nil && lj.LaunchedAt > 0 {
			launched = time.Unix(lj.LaunchedAt, 0)
		}
	}
	// Logs are keyed by the launch day (UTC); try that day and the next.
	for _, t := range []time.Time{launched, launched.Add(24 * time.Hour), launched.Add(-24 * time.Hour)} {
		key := fmt.Sprintf("logs/%s/%s.log", t.UTC().Format("2006-01-02"), runner)
		if _, err := e.s3().HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(e.out("DataBucket")), Key: &key}); err == nil {
			return key, nil
		}
	}
	return fmt.Sprintf("logs/%s/%s.log", launched.UTC().Format("2006-01-02"), runner), nil
}

func controlPlaneLogs(ctx context.Context, e *env, since time.Duration, filter string, follow bool, w io.Writer) error {
	cw := cloudwatchlogs.NewFromConfig(e.cfg)
	groups := []string{"/truner/" + e.name + "/webhook", "/truner/" + e.name + "/worker", "/truner/" + e.name + "/janitor"}
	start := time.Now().Add(-since)
	seen := map[string]bool{}
	for {
		type ev struct {
			ts    int64
			group string
			msg   string
		}
		var evs []ev
		for _, g := range groups {
			in := &cloudwatchlogs.FilterLogEventsInput{LogGroupName: aws.String(g), StartTime: aws.Int64(start.UnixMilli())}
			if filter != "" {
				in.FilterPattern = aws.String(`"` + filter + `"`)
			}
			p := cloudwatchlogs.NewFilterLogEventsPaginator(cw, in)
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					break
				}
				for _, e := range page.Events {
					id := aws.ToString(e.EventId)
					if seen[id] {
						continue
					}
					seen[id] = true
					msg := strings.TrimSpace(aws.ToString(e.Message))
					if strings.HasPrefix(msg, `{"_aws"`) || strings.HasPrefix(msg, "START ") || strings.HasPrefix(msg, "END ") || strings.HasPrefix(msg, "REPORT ") {
						continue
					}
					evs = append(evs, ev{aws.ToInt64(e.Timestamp), g[strings.LastIndex(g, "/")+1:], msg})
				}
			}
		}
		sort.Slice(evs, func(i, j int) bool { return evs[i].ts < evs[j].ts })
		for _, x := range evs {
			fmt.Fprintf(w, "%s %-7s %s\n", time.UnixMilli(x.ts).Format("15:04:05"), x.group, x.msg)
		}
		if !follow {
			return nil
		}
		start = time.Now().Add(-30 * time.Second)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
}
