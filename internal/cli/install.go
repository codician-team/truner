package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/spf13/cobra"

	"github.com/codician-team/truner/internal/assets"
	"github.com/codician-team/truner/internal/controlplane"
	"github.com/codician-team/truner/internal/deploy"
	"github.com/codician-team/truner/internal/github"
	"github.com/codician-team/truner/internal/version"
)

type installFlags struct {
	githubOwner    string
	network        string
	vpcID          string
	publicSubnets  string
	privateSubnets string
	nat            bool
	sshCIDR        string
	alertEmail     string
	dailyReport    bool
	labelPrefix    string
	runnerPolicies string
	noBake         bool
	noEcr          bool
	reconfigureApp bool
	cacheDays      int
	logDays        int
	upgradeOnly    bool
}

var stackNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]{0,40}$`)

func (a *app) installCmd() *cobra.Command {
	f := &installFlags{}
	cmd := &cobra.Command{
		Use:     "install",
		Aliases: []string{"setup", "init"},
		Short:   "Install truner in your AWS account and connect it to GitHub",
		Long: `Install deploys the truner control plane (a CloudFormation stack that costs
nothing while idle) into your AWS account, creates a private GitHub App
for your organization or account, and connects the two.

Running it again on an existing installation upgrades it and applies any
changed options.

Scripts and AI agents: pass --region and --github-owner (omit it for a
personal account) and run with --yes. The command never prompts without a
terminal. Lines starting with "ACTION REQUIRED" contain a link the user has
to open. If the command is interrupted, run it again: it continues where it
stopped and reuses a pending GitHub link.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.install(cmd.Context(), f, cmd)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.githubOwner, "github-owner", "", "GitHub organization to create the app for (empty: your personal account)")
	fl.StringVar(&f.network, "network", "create", "create a VPC (create) or use existing subnets (existing)")
	fl.StringVar(&f.vpcID, "vpc-id", "", "existing VPC (with --network existing)")
	fl.StringVar(&f.publicSubnets, "public-subnets", "", "comma-separated subnets with internet access (with --network existing)")
	fl.StringVar(&f.privateSubnets, "private-subnets", "", "comma-separated subnets for private=true jobs (with --network existing)")
	fl.BoolVar(&f.nat, "nat", false, "add private subnets behind a NAT gateway for a static egress IP (~$32/month)")
	fl.StringVar(&f.sshCIDR, "ssh-cidr", "", "allow SSH to runners from this CIDR (e.g. 203.0.113.4/32)")
	fl.StringVar(&f.alertEmail, "alert-email", "", "email for budget alerts and failures")
	fl.BoolVar(&f.dailyReport, "daily-report", false, "email a daily usage and cost report (needs --alert-email)")
	fl.StringVar(&f.labelPrefix, "label-prefix", "", "first runs-on label routed to this installation (default \"truner\")")
	fl.StringVar(&f.runnerPolicies, "runner-policy", "", "comma-separated IAM policy ARNs to attach to runner machines")
	fl.BoolVar(&f.noBake, "no-bake", false, "do not pre-build optimized images after installing")
	fl.BoolVar(&f.noEcr, "no-ecr", false, "do not create the ECR repository for Docker layer caching")
	fl.BoolVar(&f.reconfigureApp, "new-github-app", false, "create a new GitHub App even if one is configured")
	fl.IntVar(&f.cacheDays, "cache-days", 10, "days to keep magic cache entries and Docker S3 caches")
	fl.IntVar(&f.logDays, "log-days", 14, "days to keep machine and control plane logs (1, 3, 5, 7, 14, 30, 60, 90, ...)")
	return cmd
}

func (a *app) upgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade an installation to this version of truner",
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.install(cmd.Context(), &installFlags{noBake: true, upgradeOnly: true}, cmd)
		},
	}
}

func (a *app) install(ctx context.Context, f *installFlags, cmd *cobra.Command) error {
	u := a.ui
	if !stackNameRe.MatchString(a.stack) {
		return fmt.Errorf("invalid installation name %q (letters, digits and dashes; max 41)", a.stack)
	}
	u.printf("%s %s %s\n", u.bold("truner"), u.dim(version.Version), u.dim("- GitHub Actions runners in your own AWS account"))
	const steps = 5

	// 1. AWS account
	u.step(1, steps, "AWS account")
	cfg, id, opts, err := awsInteractive(ctx, u, a.aws, a.aws.Region == "" && loadState().Region == "")
	if err != nil {
		return err
	}
	a.aws = opts
	u.ok("Account %s as %s", id.Account, u.dim(id.Arn))
	u.ok("Region %s, installation %q", cfg.Region, a.stack)
	existing, err := deploy.Describe(ctx, cfg, a.stack)
	if err != nil && !errors.Is(err, deploy.ErrNoStack) {
		return err
	}
	if existing != nil {
		u.ok("Existing installation found (%s, version %s): it will be upgraded", existing.Status, existing.Outputs["Version"])
	} else if f.upgradeOnly {
		return errNotInstalled
	}

	// 2. Preflight
	u.step(2, steps, "Preflight checks")
	azs, err := availabilityZones(ctx, cfg)
	if err != nil {
		return fmt.Errorf("list availability zones: %w", err)
	}
	if len(azs) < 2 {
		return fmt.Errorf("region %s has fewer than 2 availability zones", cfg.Region)
	}
	azCount := min(len(azs), 3)
	u.ok("%d availability zones (%s)", len(azs), strings.Join(azs, ", "))
	checkQuotas(ctx, u, cfg)
	bundle, err := assets.Load(ctx)
	if err != nil {
		return err
	}
	ver := version.Version
	if version.IsDev() {
		ver = "dev-" + bundle.Digest
	}
	u.ok("Artifacts for version %s ready", ver)

	// 3. Control plane
	u.step(3, steps, "Control plane")
	bucket := deploy.ArtifactsBucketName(id.Account, cfg.Region)
	if err := deploy.EnsureBucket(ctx, cfg, bucket); err != nil {
		return err
	}
	sp := u.spin("Uploading artifacts to s3://" + bucket)
	uploads := map[string][]byte{
		"lambda/" + ver + "/" + assets.LambdaZip: bundle.Files[assets.LambdaZip],
		"templates/" + ver + "/truner.yaml":      []byte(deploy.Template),
	}
	for _, name := range []string{assets.AgentLinuxAMD64, assets.AgentLinuxARM64, assets.AgentWindowsAMD64} {
		uploads["agent/"+ver+"/"+name] = bundle.Files[name]
	}
	for key, data := range uploads {
		if err := deploy.Upload(ctx, cfg, bucket, key, data); err != nil {
			sp.done()
			return err
		}
	}
	sp.done()
	u.ok("Uploaded control plane and agent artifacts")

	params := map[string]string{
		"Version":         ver,
		"ArtifactsBucket": bucket,
		"LambdaKey":       "lambda/" + ver + "/" + assets.LambdaZip,
	}
	set := func(flag, param, value string) {
		if existing == nil || cmd.Flags().Changed(flag) {
			params[param] = value
		}
	}
	set("network", "NetworkMode", f.network)
	set("vpc-id", "ExistingVpcId", f.vpcID)
	set("public-subnets", "ExistingPublicSubnetIds", f.publicSubnets)
	set("private-subnets", "ExistingPrivateSubnetIds", f.privateSubnets)
	set("nat", "EnableNat", strconv.FormatBool(f.nat))
	set("ssh-cidr", "SshCidr", f.sshCIDR)
	set("alert-email", "AlertEmail", f.alertEmail)
	set("daily-report", "DailyReport", strconv.FormatBool(f.dailyReport))
	set("label-prefix", "LabelPrefix", firstNonEmpty(f.labelPrefix, "truner"))
	set("runner-policy", "RunnerPolicyArns", f.runnerPolicies)
	set("no-ecr", "EnableEcrCache", strconv.FormatBool(!f.noEcr))
	set("cache-days", "CacheRetentionDays", strconv.Itoa(f.cacheDays))
	set("log-days", "LogRetentionDays", strconv.Itoa(f.logDays))
	if existing == nil {
		params["AvailabilityZoneCount"] = strconv.Itoa(azCount)
	}
	if f.network == "existing" && existing == nil && (f.vpcID == "" || f.publicSubnets == "") {
		return errors.New("--network existing needs --vpc-id and --public-subnets")
	}
	templateURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/templates/%s/truner.yaml", bucket, cfg.Region, ver)
	start := time.Now()
	u.info("Deploying CloudFormation stack %q (takes 2-4 minutes)...", a.stack)
	st, err := deploy.Deploy(ctx, cfg, a.stack, templateURL, params, &indentWriter{u: u})
	if err != nil {
		return err
	}
	u.ok("Control plane ready in %s", dur(time.Since(start)))
	saved := loadState()
	saved.Stack, saved.Region, saved.Profile = a.stack, cfg.Region, a.aws.Profile
	saveState(saved)
	e := &env{cfg: cfg, name: a.stack, stack: st}
	webhookURL := st.Outputs["WebhookUrl"]

	// 4. GitHub App
	u.step(4, steps, "GitHub App")
	gh, err := e.github(ctx)
	if err != nil && !errors.Is(err, controlplane.ErrNoApp) {
		return err
	}
	if gh != nil && !f.reconfigureApp {
		if info, err := gh.GetApp(ctx); err == nil {
			u.ok("Using GitHub App %s (%s)", info.Name, info.HTMLURL)
			if err := gh.UpdateHookConfig(ctx, webhookURL, ""); err != nil {
				u.warn("could not update the app's webhook URL: %v", err)
			}
		} else {
			u.warn("the stored GitHub App does not work (%v); creating a new one", err)
			gh = nil
		}
	}
	created := false
	if gh == nil || f.reconfigureApp {
		owner := f.githubOwner
		if !cmd.Flags().Changed("github-owner") {
			owner = u.ask("GitHub organization to use truner with (empty for your personal account)", "")
		}
		setup, err := connectApp(ctx, u, e, owner, f.reconfigureApp)
		if err != nil {
			return fmt.Errorf("GitHub App: %w", err)
		}
		if gh, err = e.github(ctx); err != nil {
			return err
		}
		u.ok("Created GitHub App %s (owned by %s)", setup.AppName, setup.AppOwner)
		created = true
	}
	app := gh.App()
	insts := waitForInstallation(ctx, u, gh, app.Slug, !created)
	if len(insts) == 0 {
		u.warn("the app is not installed yet; install it later at https://github.com/apps/%s/installations/new", app.Slug)
	}
	for _, in := range insts {
		sel := "selected repositories"
		if in.RepositorySelection == "all" {
			sel = "all repositories"
		}
		u.ok("Installed on %s (%s)", in.Account.Login, sel)
	}

	// 5. Finish
	u.step(5, steps, "Finishing")
	if resp, err := e.invoke(ctx, controlplane.AdminRequest{Truner: "ping"}, false); err != nil || !resp.OK {
		msg := ""
		if resp != nil {
			msg = resp.Message
		}
		u.warn("control plane check failed: %v %s (run `truner doctor`)", err, msg)
	} else {
		u.ok("Control plane is healthy (version %s)", resp.Version)
	}
	e.invoke(ctx, controlplane.AdminRequest{Truner: "mirror-runner"}, true)
	if !f.noBake {
		for _, arch := range []string{"x64", "arm64"} {
			e.invoke(ctx, controlplane.AdminRequest{Truner: "bake", Image: "ubuntu24", Arch: arch}, true)
		}
		u.ok("Building optimized ubuntu24 images (x64, arm64) in the background (~10 minutes)")
		u.info("%s", u.dim("Jobs work right away; they boot faster once the images are ready."))
	}
	prefix := firstNonEmpty(st.Parameters["LabelPrefix"], "truner")
	u.printf("\n%s\n\n", u.green(u.bold("truner is ready!")))
	u.printf("Use it in any workflow of the repositories the app is installed on:\n\n")
	u.printf("    jobs:\n      build:\n        runs-on: %s/cpu=4/run=${{ github.run_id }}\n\n", prefix)
	u.printf("More shapes:  %s/runner=8cpu-linux-arm64   %s/cpu=16/ram=64/spot=false\n", prefix, prefix)
	u.printf("              %s/image=ubuntu24-full       %s/runner=gpu-nvidia   %s/runner=4cpu-windows-x64\n\n", prefix, prefix, prefix)
	u.printf("Next:  truner doctor   (verify everything)    truner status   truner jobs   truner costs\n")
	u.printf("Docs:  https://github.com/%s#readme\n", version.Repo)
	return nil
}

func availabilityZones(ctx context.Context, cfg aws.Config) ([]string, error) {
	out, err := ec2.NewFromConfig(cfg).DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{
		Filters: []ec2types.Filter{{Name: aws.String("zone-type"), Values: []string{"availability-zone"}}, {Name: aws.String("state"), Values: []string{"available"}}},
	})
	if err != nil {
		return nil, err
	}
	var azs []string
	for _, z := range out.AvailabilityZones {
		azs = append(azs, aws.ToString(z.ZoneName))
	}
	return azs, nil
}

// Service quota codes for EC2 vCPUs.
const (
	quotaOnDemandStandard = "L-1216C47A"
	quotaSpotStandard     = "L-34B43A08"
	quotaOnDemandGPU      = "L-DB2E81BA"
	quotaSpotGPU          = "L-3819A6DF"
)

func quotaValue(ctx context.Context, cfg aws.Config, code string) (float64, error) {
	out, err := servicequotas.NewFromConfig(cfg).GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{ServiceCode: aws.String("ec2"), QuotaCode: aws.String(code)})
	if err != nil {
		return 0, err
	}
	return aws.ToFloat64(out.Quota.Value), nil
}

func checkQuotas(ctx context.Context, u *ui, cfg aws.Config) {
	od, err1 := quotaValue(ctx, cfg, quotaOnDemandStandard)
	spot, err2 := quotaValue(ctx, cfg, quotaSpotStandard)
	if err1 != nil || err2 != nil {
		u.warn("could not read EC2 vCPU quotas (fine; `truner doctor` checks again later)")
		return
	}
	line := fmt.Sprintf("EC2 vCPU quotas: %.0f spot, %.0f on-demand", spot, od)
	if spot < 64 || od < 64 {
		u.warn("%s - enough for only a few concurrent jobs. Request more with: truner quota --request 512", line)
		return
	}
	u.ok("%s", line)
}

// indentWriter indents stack events under the current step.
type indentWriter struct{ u *ui }

func (w *indentWriter) Write(p []byte) (int, error) {
	for _, l := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		w.u.printf("  %s\n", w.u.dim(l))
	}
	return len(p), nil
}

func (a *app) uninstallCmd() *cobra.Command {
	var keepImages bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove truner from your AWS account",
		Long:  "Terminates all runner machines, deletes baked images, the cache bucket and the CloudFormation stack.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			u := a.ui
			e, err := a.connect(ctx)
			if err != nil {
				return err
			}
			u.printf("This removes the truner installation %q in %s, including all cached data.\n", e.name, e.cfg.Region)
			if !u.yes {
				if got := u.ask("Type the installation name to confirm", ""); got != e.name {
					return errors.New("aborted")
				}
			}
			var appInfo *github.AppInfo
			if gh, err := e.github(ctx); err == nil {
				appInfo, _ = gh.GetApp(ctx)
			}
			cp := controlplane.NewWithConfig(&controlplane.Env{Stack: e.name, Region: e.cfg.Region, Table: e.out("Table")}, e.cfg)
			insts, err := cp.ManagedInstances(ctx)
			if err == nil && len(insts) > 0 {
				var ids []string
				for _, i := range insts {
					ids = append(ids, aws.ToString(i.InstanceId))
				}
				if err := cp.TerminateInstances(ctx, ids...); err != nil {
					u.warn("terminate instances: %v", err)
				} else {
					u.ok("Terminated %d machine(s)", len(ids))
				}
			}
			if !keepImages {
				imgs, err := e.ec2().DescribeImages(ctx, &ec2.DescribeImagesInput{Owners: []string{"self"}, Filters: []ec2types.Filter{{Name: aws.String("tag:truner:stack"), Values: []string{e.name}}}})
				if err == nil {
					for _, img := range imgs.Images {
						cp.DeleteImage(ctx, img)
					}
					u.ok("Deleted %d baked image(s)", len(imgs.Images))
				}
			}
			if b := e.out("DataBucket"); b != "" {
				sp := u.spin("Emptying s3://" + b)
				n, err := deploy.EmptyBucket(ctx, e.cfg, b, "")
				sp.done()
				if err != nil {
					return err
				}
				u.ok("Emptied the data bucket (%d objects)", n)
			}
			u.info("Deleting the CloudFormation stack...")
			if err := deploy.Delete(ctx, e.cfg, e.name, &indentWriter{u: u}); err != nil {
				return err
			}
			u.ok("Stack deleted")
			param := firstNonEmpty(e.out("AppParameter"), "/truner/"+e.name+"/github-app")
			ssm.NewFromConfig(e.cfg).DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: &param})
			if others, err := deploy.ListStacks(ctx, e.cfg); err == nil && len(others) == 0 {
				if id, err := whoami(ctx, e.cfg); err == nil {
					deploy.DeleteBucket(ctx, e.cfg, deploy.ArtifactsBucketName(id.Account, e.cfg.Region))
				}
			}
			if st := loadState(); st.Stack == e.name {
				os.Remove(statePath())
			}
			u.printf("\n%s\n", u.green("truner has been removed."))
			if appInfo != nil {
				u.printf("GitHub Apps cannot be deleted through the API; remove %q at:\n  %s\n", appInfo.Name, appSettingsURL(appInfo))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&keepImages, "keep-images", false, "keep baked AMIs")
	return cmd
}

func appSettingsURL(a *github.AppInfo) string {
	if a.Owner.Login != "" && !strings.EqualFold(a.Owner.Login, "") {
		return fmt.Sprintf("https://github.com/organizations/%s/settings/apps/%s/advanced (or https://github.com/settings/apps/%s/advanced for personal accounts)", a.Owner.Login, a.Slug, a.Slug)
	}
	return "https://github.com/settings/apps/" + a.Slug + "/advanced"
}
