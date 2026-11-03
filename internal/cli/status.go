package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/spf13/cobra"

	"github.com/codician-team/truner/internal/config"
	"github.com/codician-team/truner/internal/controlplane"
	"github.com/codician-team/truner/internal/deploy"
	"github.com/codician-team/truner/internal/github"
	"github.com/codician-team/truner/internal/version"
)

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the state of the installation",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			u := a.ui
			e, err := a.connect(ctx)
			if err != nil {
				return err
			}
			u.printf("%s  %s  %s %s\n\n", u.bold("truner "+e.out("Version")), e.name, e.cfg.Region, u.dim(e.stack.Status))
			if gh, err := e.github(ctx); err == nil {
				if info, err := gh.GetApp(ctx); err == nil {
					u.printf("GitHub App   %s %s\n", info.Name, u.dim(info.HTMLURL))
				}
				if insts, err := gh.ListInstallations(ctx); err == nil {
					var names []string
					for _, in := range insts {
						names = append(names, in.Account.Login+" ("+in.RepositorySelection+")")
					}
					u.printf("Installed on %s\n", firstNonEmpty(strings.Join(names, ", "), u.yellow("nothing yet")))
				}
				if ds, err := gh.ListHookDeliveries(ctx, 50); err == nil && len(ds) > 0 {
					failed := 0
					for _, d := range ds {
						if d.StatusCode >= 300 || d.StatusCode == 0 {
							failed++
						}
					}
					t, _ := time.Parse(time.RFC3339, ds[0].DeliveredAt)
					u.printf("Webhooks     last %s (%d), %d of the last %d failed\n", ago(t), ds[0].StatusCode, failed, len(ds))
				}
			} else {
				u.printf("GitHub App   %s\n", u.yellow(err.Error()))
			}
			u.printf("Webhook URL  %s\n", e.out("WebhookUrl"))
			u.printf("Labels       runs-on: %s\n", exampleLabel(e.labelPrefix(), "cpu=4", "<job>"))
			if ip := e.out("StaticEgressIp"); ip != "" {
				u.printf("Egress IP    %s %s\n", ip, u.dim("(private=true jobs)"))
			}

			cp := controlplane.NewWithConfig(&controlplane.Env{Stack: e.name, Region: e.cfg.Region}, e.cfg)
			insts, err := cp.ManagedInstances(ctx, "pending", "running", "stopped", "stopping")
			if err == nil {
				roles := map[string]int{}
				spot := 0
				for _, i := range insts {
					r := controlplane.InstanceTag(i, controlplane.TagRole)
					if r == "pool" {
						r = "pool-" + string(i.State.Name)
					}
					roles[r]++
					if i.InstanceLifecycle == ec2types.InstanceLifecycleTypeSpot {
						spot++
					}
				}
				u.printf("Machines     %d running job(s) (%d spot), %d pool ready, %d preparing/building\n",
					roles["runner"], spot, roles["pool-stopped"], roles["pool-running"]+roles["pool-pending"]+roles["builder"])
			}
			q := sqs.NewFromConfig(e.cfg)
			depth := func(url string) string {
				out, err := q.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(url), AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameApproximateNumberOfMessages, sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed}})
				if err != nil {
					return "?"
				}
				return out.Attributes["ApproximateNumberOfMessages"] + "+" + out.Attributes["ApproximateNumberOfMessagesDelayed"] + " delayed"
			}
			u.printf("Queue        %s waiting, dead-letter %s\n", depth(e.out("LaunchQueueUrl")), depth(e.out("DeadLetterQueueUrl")))

			store := e.store()
			jobs, _ := store.JobsInColl(ctx, controlplane.JobColl(time.Now()))
			var waits []float64
			cost := 0.0
			for _, j := range jobs {
				if j.StartedAt > 0 && j.StartedAt >= j.TS {
					waits = append(waits, float64(j.StartedAt-j.TS))
				}
				cost += j.Cost
			}
			sort.Float64s(waits)
			med := "-"
			if len(waits) > 0 {
				med = fmt.Sprintf("%.0fs", waits[len(waits)/2])
			}
			u.printf("Today        %d job(s), median queue-to-start %s, %s\n", len(jobs), med, money(cost))
			if it, err := store.GetItem(ctx, "spend#"+time.Now().UTC().Format("2006-01")); err == nil && it != nil {
				u.printf("This month   %s estimated\n", money(it.Float("total")))
			}
			if items, err := store.ItemsInColl(ctx, "bakes"); err == nil && len(items) > 0 {
				var parts []string
				for _, it := range items {
					s := it.Str("image") + "/" + it.Str("arch") + " " + it.Str("status")
					if it.Str("status") == "failed" {
						s = u.red(s)
					}
					parts = append(parts, s)
				}
				sort.Strings(parts)
				u.printf("Images       %s\n", strings.Join(parts, ", "))
			}
			u.printf("Dashboard    %s\n", u.dim(e.out("DashboardUrl")))
			// connect already warned about an older CLI.
			if older, msg := cliSkew(e.out("Version")); msg != "" && !older {
				u.printf("\n%s %s\n", u.yellow("!"), msg)
			}
			return nil
		},
	}
}

func (a *app) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the installation end to end and explain any problem",
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.doctor(cmd.Context())
		},
	}
}

func (a *app) doctor(ctx context.Context) error {
	u := a.ui
	problems := 0
	bad := func(format string, args ...any) { problems++; u.bad(format, args...) }
	u.println(u.bold("AWS"))
	e, err := a.connect(ctx)
	if err != nil {
		bad("%v", err)
		return nil
	}
	if id, err := whoami(ctx, e.cfg); err == nil {
		u.ok("credentials: %s", id.Arn)
	}
	switch {
	case strings.HasSuffix(e.stack.Status, "_COMPLETE") && !strings.Contains(e.stack.Status, "ROLLBACK"):
		u.ok("stack %s is %s (version %s)", e.name, e.stack.Status, e.out("Version"))
	default:
		bad("stack %s is %s: %s", e.name, e.stack.Status, e.stack.Reason)
	}
	if older, msg := cliSkew(e.out("Version")); msg != "" && !older {
		u.warn("%s", msg)
	}
	if latest := newerRelease(ctx); latest != "" {
		u.warn("this CLI is %s and truner %s is out: `truner upgrade` updates the CLI and the installation", version.Version, latest)
	}
	if locked, err := deploy.Locked(ctx, e.cfg, e.name); err == nil && !locked {
		u.warn("a truner CLI older than v0.1.5 can still downgrade this installation: run `truner upgrade` once to lock it")
	}
	resp, err := e.invoke(ctx, controlplane.AdminRequest{Truner: "ping"}, false)
	switch {
	case err != nil:
		bad("control plane unreachable: %v", err)
	case !resp.OK:
		bad("control plane: %s", resp.Message)
	default:
		u.ok("control plane responds (version %s)", resp.Version)
	}
	for _, q := range []struct{ code, name string }{{quotaSpotStandard, "spot"}, {quotaOnDemandStandard, "on-demand"}, {quotaSpotGPU, "spot GPU"}, {quotaOnDemandGPU, "on-demand GPU"}} {
		v, err := quotaValue(ctx, e.cfg, q.code)
		if err != nil {
			continue
		}
		switch {
		case v < 32 && !strings.Contains(q.name, "GPU"):
			bad("EC2 %s vCPU quota is only %.0f: run `truner quota --request 512`", q.name, v)
		case v < 8 && strings.Contains(q.name, "GPU"):
			u.warn("EC2 %s vCPU quota is %.0f (GPU runners need at least 4-8; `truner quota --gpu --request 64`)", q.name, v)
		default:
			u.ok("EC2 %s vCPU quota: %.0f", q.name, v)
		}
	}
	if subnets := e.stack.Parameters["ExistingPublicSubnetIds"]; e.stack.Parameters["NetworkMode"] == "existing" && subnets != "" {
		out, err := e.ec2().DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: strings.Split(subnets, ",")})
		if err != nil {
			bad("subnets: %v", err)
		} else {
			for _, s := range out.Subnets {
				if !aws.ToBool(s.MapPublicIpOnLaunch) {
					u.warn("subnet %s does not assign public IPs; runners need a NAT route for internet access", aws.ToString(s.SubnetId))
				}
			}
		}
	}
	if out, err := sqs.NewFromConfig(e.cfg).GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(e.out("DeadLetterQueueUrl")), AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameApproximateNumberOfMessages}}); err == nil {
		if n := out.Attributes["ApproximateNumberOfMessages"]; n != "0" {
			bad("%s launch request(s) in the dead-letter queue (check `truner logs --control-plane`)", n)
		} else {
			u.ok("no failed launch requests")
		}
	}
	if obj, err := e.s3().GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(e.out("DataBucket")), Key: aws.String(controlplane.GlobalConfigKey)}); err == nil {
		data, _ := io.ReadAll(obj.Body)
		obj.Body.Close()
		if _, err := config.ParseGlobal(data); err != nil {
			bad("global config is invalid (the last good version is in use): %v", err)
		} else {
			u.ok("global config is valid")
		}
	}
	jobs, _ := e.store().JobsInColl(ctx, controlplane.JobColl(time.Now()))
	yesterday, _ := e.store().JobsInColl(ctx, controlplane.JobColl(time.Now().Add(-24*time.Hour)))
	failed := 0
	for _, j := range append(jobs, yesterday...) {
		if j.Status == controlplane.StatusFailed {
			failed++
			if failed <= 3 {
				bad("job %s / %s could not start: %s", j.FullName(), j.Name, j.Error)
			}
		}
	}
	if failed == 0 {
		u.ok("no launch failures in the last 24h (%d jobs)", len(jobs)+len(yesterday))
	}

	u.println(u.bold("\nGitHub"))
	gh, err := e.github(ctx)
	if err != nil {
		bad("%v", err)
		return summary(u, problems)
	}
	info, err := gh.GetApp(ctx)
	if err != nil {
		bad("GitHub App credentials do not work: %v (fix with `truner install --new-github-app`)", err)
		return summary(u, problems)
	}
	u.ok("app %s (%s)", info.Name, info.HTMLURL)
	for perm, want := range controlplane.AppPermissions {
		if have := info.Permissions[perm]; have != want && !(want == "read" && have == "write") {
			bad("app permission %s is %q, needs %q (edit the app's permissions on GitHub)", perm, have, want)
		}
	}
	hasEvent := false
	for _, ev := range info.Events {
		hasEvent = hasEvent || ev == "workflow_job"
	}
	if !hasEvent {
		bad("app is not subscribed to the workflow_job event")
	}
	insts, err := gh.ListInstallations(ctx)
	switch {
	case err != nil:
		bad("list installations: %v", err)
	case len(insts) == 0:
		bad("the app is not installed anywhere: https://github.com/apps/%s/installations/new", info.Slug)
	default:
		for _, in := range insts {
			if in.SuspendedAt != nil {
				bad("installation on %s is suspended", in.Account.Login)
			} else {
				u.ok("installed on %s (%s repositories)", in.Account.Login, in.RepositorySelection)
			}
		}
	}
	checkDeliveries(ctx, u, gh, &problems)

	u.println(u.bold("\nJobs"))
	checkWaitingJobs(ctx, u, e, gh, append(jobs, yesterday...), &problems)
	return summary(u, problems)
}

func checkDeliveries(ctx context.Context, u *ui, gh *github.Client, problems *int) {
	ds, err := gh.ListHookDeliveries(ctx, 100)
	if err != nil {
		u.warn("could not list webhook deliveries: %v", err)
		return
	}
	if len(ds) == 0 {
		u.warn("no webhook deliveries yet - run a workflow with runs-on: truner")
		return
	}
	failed := map[int]int{}
	for _, d := range ds {
		if d.StatusCode >= 300 || d.StatusCode == 0 {
			failed[d.StatusCode]++
		}
	}
	if len(failed) == 0 {
		u.ok("last %d webhook deliveries succeeded", len(ds))
		return
	}
	for code, n := range failed {
		hint := ""
		switch code {
		case 401:
			hint = " (webhook secret mismatch: run `truner install --new-github-app`)"
		case 0:
			hint = " (endpoint unreachable)"
		case 503:
			hint = " (control plane has no GitHub App credentials)"
		}
		*problems++
		u.bad("%d of the last %d webhook deliveries failed with status %d%s", n, len(ds), code, hint)
	}
}

func summary(u *ui, problems int) error {
	u.println()
	if problems == 0 {
		u.println(u.green("Everything looks good."))
		return nil
	}
	return fmt.Errorf("%d problem(s) found", problems)
}
