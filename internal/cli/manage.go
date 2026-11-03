package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	"github.com/spf13/cobra"

	"github.com/codician-team/truner/internal/cachestore"
	"github.com/codician-team/truner/internal/config"
	"github.com/codician-team/truner/internal/controlplane"
)

// ---------------------------------------------------------------- images

func (a *app) imagesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "images", Short: "List, build and delete optimized runner images (AMIs)"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List baked images and builds",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			e, err := a.connect(ctx)
			if err != nil {
				return err
			}
			out, err := e.ec2().DescribeImages(ctx, &ec2.DescribeImagesInput{Owners: []string{"self"}, Filters: []ec2types.Filter{{Name: aws.String("tag:" + controlplane.TagStack), Values: []string{e.name}}}})
			if err != nil {
				return err
			}
			var rows [][]string
			for _, img := range out.Images {
				var tag = func(k string) string {
					for _, t := range img.Tags {
						if aws.ToString(t.Key) == k {
							return aws.ToString(t.Value)
						}
					}
					return ""
				}
				created, _ := time.Parse(time.RFC3339, aws.ToString(img.CreationDate))
				rows = append(rows, []string{tag(controlplane.TagImage), tag(controlplane.TagArch), aws.ToString(img.ImageId), string(img.State), ago(created), tag("truner:runner-version")})
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i][0]+rows[i][1]+rows[i][4] < rows[j][0]+rows[j][1]+rows[j][4] })
			if len(rows) == 0 {
				a.ui.println("No baked images yet. Images are built automatically on first use, or now with `truner images build ubuntu24`.")
			} else {
				a.ui.table([]string{"IMAGE", "ARCH", "AMI", "STATE", "CREATED", "RUNNER"}, rows)
			}
			if micro, _ := e.store().ItemsInColl(ctx, "micro-images"); len(micro) > 0 {
				a.ui.println()
				var mrows [][]string
				for _, it := range micro {
					mrows = append(mrows, []string{fmt.Sprintf("micro, %d vCPU", it.Int("vcpu")), it.Str("status"), firstNonEmpty(it.Str("imageVersion"), "-"),
						firstNonEmpty(it.Str("agent"), "-"), firstNonEmpty(it.Str("runner"), "-"), ago(time.Unix(it.Int("ts"), 0)), it.Str("error")})
				}
				sort.Slice(mrows, func(i, j int) bool { return mrows[i][0] < mrows[j][0] })
				a.ui.table([]string{"MICROVM IMAGE", "STATUS", "VERSION", "TRUNER", "RUNNER", "UPDATED", "ERROR"}, mrows)
			}
			items, _ := e.store().ItemsInColl(ctx, "bakes")
			if len(items) > 0 {
				a.ui.println()
				var brows [][]string
				for _, it := range items {
					brows = append(brows, []string{it.Str("image"), it.Str("arch"), it.Str("status"), ago(time.Unix(it.Int("ts"), 0)), it.Str("builder"), it.Str("error")})
				}
				a.ui.table([]string{"BUILD", "ARCH", "STATUS", "STARTED", "BUILDER", "ERROR"}, brows)
			}
			return nil
		},
	})
	var arch string
	build := &cobra.Command{
		Use:   "build IMAGE",
		Short: "Build (or rebuild) an optimized image, e.g. ubuntu24, ubuntu24-full",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			e, err := a.connect(ctx)
			if err != nil {
				return err
			}
			archs := []string{arch}
			if arch == "all" {
				archs = []string{config.ArchX64, config.ArchARM64}
			}
			for _, ar := range archs {
				resp, err := e.invoke(ctx, controlplane.AdminRequest{Truner: "bake", Image: args[0], Arch: ar, Force: true}, false)
				if err != nil {
					return err
				}
				if !resp.OK {
					return errors.New(resp.Message)
				}
				a.ui.ok("%s", resp.Message)
			}
			a.ui.info("Follow progress with `truner images list`. Builds take 5-20 minutes.")
			return nil
		},
	}
	build.Flags().StringVar(&arch, "arch", "x64", "x64, arm64 or all")
	cmd.AddCommand(build)
	cmd.AddCommand(&cobra.Command{
		Use:   "delete AMI-ID",
		Short: "Delete a baked image and its snapshots",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			e, err := a.connect(ctx)
			if err != nil {
				return err
			}
			out, err := e.ec2().DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{args[0]}})
			if err != nil || len(out.Images) == 0 {
				return fmt.Errorf("image %s not found", args[0])
			}
			cp := controlplane.NewWithConfig(&controlplane.Env{Stack: e.name, Region: e.cfg.Region}, e.cfg)
			if err := cp.DeleteImage(ctx, out.Images[0]); err != nil {
				return err
			}
			a.ui.ok("deleted %s", args[0])
			return nil
		},
	})
	return cmd
}

// ---------------------------------------------------------------- cache

func (a *app) cacheCmd() *cobra.Command {
	var repo string
	cmd := &cobra.Command{Use: "cache", Short: "Inspect and clean the magic cache (S3)"}
	cmd.PersistentFlags().StringVar(&repo, "repo", "", "only this repository (owner/name)")
	list := func(ctx context.Context) (*env, []cachestore.Entry, error) {
		e, err := a.connect(ctx)
		if err != nil {
			return nil, nil, err
		}
		st := cachestore.NewS3(e.s3(), e.out("DataBucket"))
		ents, err := cachestore.ListEntries(ctx, st, repo)
		return e, ents, err
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "ls",
		Short: "List cache entries",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, ents, err := list(cmd.Context())
			if err != nil {
				return err
			}
			sort.Slice(ents, func(i, j int) bool { return ents[i].LastModified.After(ents[j].LastModified) })
			var rows [][]string
			var total int64
			for _, en := range ents {
				total += en.Size
				rows = append(rows, []string{en.Repo, en.Scope, en.Key, humanBytes(en.Size), ago(en.LastModified)})
			}
			if len(rows) == 0 {
				a.ui.println("The cache is empty.")
				return nil
			}
			a.ui.table([]string{"REPOSITORY", "REF", "KEY", "SIZE", "SAVED"}, rows)
			a.ui.printf("\n%d entries, %s\n", len(ents), humanBytes(total))
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "stats",
		Short: "Show cache size per repository",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, ents, err := list(cmd.Context())
			if err != nil {
				return err
			}
			size, count := map[string]int64{}, map[string]int{}
			for _, en := range ents {
				size[en.Repo] += en.Size
				count[en.Repo]++
			}
			var repos []string
			for r := range size {
				repos = append(repos, r)
			}
			sort.Slice(repos, func(i, j int) bool { return size[repos[i]] > size[repos[j]] })
			var rows [][]string
			for _, r := range repos {
				rows = append(rows, []string{r, strconv.Itoa(count[r]), humanBytes(size[r])})
			}
			a.ui.table([]string{"REPOSITORY", "ENTRIES", "SIZE"}, rows)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "rm KEY-PREFIX",
		Short: "Delete cache entries whose key starts with KEY-PREFIX",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			e, ents, err := list(ctx)
			if err != nil {
				return err
			}
			n := 0
			for _, en := range ents {
				if strings.HasPrefix(en.Key, args[0]) {
					if _, err := e.s3().DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(e.out("DataBucket")), Key: aws.String(en.ObjectKey)}); err != nil {
						return err
					}
					n++
				}
			}
			a.ui.ok("deleted %d entries", n)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "purge",
		Short: "Delete all cache entries (of --repo, or everything)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			e, ents, err := list(ctx)
			if err != nil {
				return err
			}
			what := "ALL repositories"
			if repo != "" {
				what = repo
			}
			if !a.ui.confirm(fmt.Sprintf("Delete %d cache entries of %s?", len(ents), what), false) {
				return errors.New("aborted")
			}
			for _, en := range ents {
				e.s3().DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(e.out("DataBucket")), Key: aws.String(en.ObjectKey)})
			}
			a.ui.ok("deleted %d entries", len(ents))
			return nil
		},
	})
	return cmd
}

// ---------------------------------------------------------------- config

const exampleGlobalConfig = `# truner account-wide configuration (truner config edit)
# Every key is optional. Docs: https://github.com/codician-team/truner/blob/main/docs/configuration.md
#
# defaults:            # applied to every job
#   disk: 60
#   spot: true
#
# runners:             # reusable machine shapes: runs-on: truner/runner=big
#   big:
#     extends: 16cpu-linux-x64
#     image: ubuntu24-full
#     disk: 200
#
# images:              # your own AMIs: runs-on: truner/image=my-image
#   my-image:
#     name: "my-golden-ami-*"
#     owner: "123456789012"
#     profile: none
#
# pools:               # warm machines for near-instant starts: runs-on: truner/pool=fast
#   fast:
#     runner: 4cpu-linux-x64
#     size: 2
#
# access:
#   repos: ["my-org/*"]
#
# limits:
#   maxInstances: 100
#   monthlyBudgetUsd: 500
`

func (a *app) configCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "View and change the account-wide configuration"}
	get := func(ctx context.Context, e *env) ([]byte, error) {
		out, err := e.s3().GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(e.out("DataBucket")), Key: aws.String(controlplane.GlobalConfigKey)})
		if err != nil {
			if strings.Contains(err.Error(), "NoSuchKey") {
				return nil, nil
			}
			return nil, err
		}
		defer out.Body.Close()
		var b bytes.Buffer
		_, err = b.ReadFrom(out.Body)
		return b.Bytes(), err
	}
	put := func(ctx context.Context, e *env, data []byte) error {
		if _, err := config.ParseGlobal(data); err != nil {
			return err
		}
		_, err := e.s3().PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(e.out("DataBucket")), Key: aws.String(controlplane.GlobalConfigKey), Body: bytes.NewReader(data), ContentType: aws.String("text/yaml")})
		return err
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print the account-wide configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := a.connect(cmd.Context())
			if err != nil {
				return err
			}
			data, err := get(cmd.Context(), e)
			if err != nil {
				return err
			}
			if len(data) == 0 {
				fmt.Print(exampleGlobalConfig)
				return nil
			}
			fmt.Print(string(data))
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "edit",
		Short: "Edit the account-wide configuration in your editor",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			e, err := a.connect(ctx)
			if err != nil {
				return err
			}
			data, err := get(ctx, e)
			if err != nil {
				return err
			}
			if len(data) == 0 {
				data = []byte(exampleGlobalConfig)
			}
			tmp := filepath.Join(os.TempDir(), "truner-config.yml")
			if err := os.WriteFile(tmp, data, 0o600); err != nil {
				return err
			}
			defer os.Remove(tmp)
			for {
				editor := firstNonEmpty(os.Getenv("VISUAL"), os.Getenv("EDITOR"))
				if editor == "" {
					editor = "vi"
					if runtime.GOOS == "windows" {
						editor = "notepad"
					}
				}
				parts := strings.Fields(editor)
				c := exec.Command(parts[0], append(parts[1:], tmp)...)
				c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
				if err := c.Run(); err != nil {
					return fmt.Errorf("editor: %w", err)
				}
				edited, err := os.ReadFile(tmp)
				if err != nil {
					return err
				}
				if bytes.Equal(edited, data) {
					a.ui.info("No changes.")
					return nil
				}
				if err := put(ctx, e, edited); err != nil {
					a.ui.bad("%v", err)
					if a.ui.confirm("Edit again?", true) {
						continue
					}
					return errors.New("configuration not saved")
				}
				a.ui.ok("Configuration saved; it applies to new jobs within a minute.")
				return nil
			}
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "apply FILE",
		Short: "Validate and upload a configuration file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			e, err := a.connect(cmd.Context())
			if err != nil {
				return err
			}
			if err := put(cmd.Context(), e, data); err != nil {
				return err
			}
			a.ui.ok("Configuration saved; it applies to new jobs within a minute.")
			return nil
		},
	})
	var repoMode bool
	validate := &cobra.Command{
		Use:   "validate FILE",
		Short: "Check a global config file, or a repository's .github/truner.yml with --repo",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			if repoMode || strings.Contains(filepath.ToSlash(args[0]), ".github/") {
				_, err = config.ParseRepo(data)
			} else {
				_, err = config.ParseGlobal(data)
			}
			if err != nil {
				return err
			}
			a.ui.ok("%s is valid", args[0])
			return nil
		},
	}
	validate.Flags().BoolVar(&repoMode, "repo", false, "validate as a repository config")
	cmd.AddCommand(validate)
	return cmd
}

// ---------------------------------------------------------------- pools

func (a *app) poolsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pools",
		Short: "Show warm pools (configure them with `truner config edit`)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			e, err := a.connect(ctx)
			if err != nil {
				return err
			}
			g, err := loadGlobalConfig(ctx, e)
			if err != nil {
				return err
			}
			if len(g.Pools) == 0 {
				a.ui.println("No pools configured. Add one with `truner config edit`, e.g.:\n\npools:\n  fast:\n    runner: 4cpu-linux-x64\n    size: 2\n\nthen use runs-on: truner/pool=fast")
				return nil
			}
			cp := controlplane.NewWithConfig(&controlplane.Env{Stack: e.name, Region: e.cfg.Region}, e.cfg)
			insts, err := cp.ManagedInstances(ctx)
			if err != nil {
				return err
			}
			var rows [][]string
			names := make([]string, 0, len(g.Pools))
			for n := range g.Pools {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, name := range names {
				p := g.Pools[name]
				counts := map[string]int{}
				itype := ""
				for _, i := range insts {
					if controlplane.InstanceTag(i, controlplane.TagPool) == name {
						counts[controlplane.InstanceTag(i, controlplane.TagPoolState)+"/"+string(i.State.Name)]++
						itype = string(i.InstanceType)
					}
				}
				spec, _ := config.Resolver{Global: g}.ResolvePool(name)
				summary := ""
				if spec != nil {
					summary = spec.Summary()
				}
				rows = append(rows, []string{name, strconv.Itoa(p.Size), strconv.Itoa(counts["ready/stopped"]), strconv.Itoa(counts["preparing/running"] + counts["preparing/pending"] + counts["preparing/stopped"]), strconv.Itoa(counts["claimed/running"] + counts["claimed/pending"]), firstNonEmpty(itype, "-"), summary})
			}
			a.ui.table([]string{"POOL", "SIZE", "READY", "PREPARING", "IN USE", "TYPE", "SPEC"}, rows)
			return nil
		},
	}
}

// ---------------------------------------------------------------- connect

func (a *app) connectCmd() *cobra.Command {
	var useSSH bool
	cmd := &cobra.Command{
		Use:   "connect JOB-ID|INSTANCE-ID",
		Short: "Open a shell on a runner machine (SSM Session Manager, or SSH)",
		Long: `Connect opens a shell on the machine running a job. Combine with the
keep=30m label to inspect a machine after its job finished:

  runs-on: truner/cpu=4/keep=30m/run=${{ github.run_id }}`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			e, err := a.connect(ctx)
			if err != nil {
				return err
			}
			id := args[0]
			if !strings.HasPrefix(id, "i-") {
				n, err := strconv.ParseInt(id, 10, 64)
				if err != nil {
					return fmt.Errorf("expected a job id or instance id")
				}
				j, err := e.store().GetJob(ctx, n)
				if err != nil || j == nil {
					return fmt.Errorf("job %s not found", id)
				}
				id = firstNonEmpty(j.RanOnInstance, j.InstanceID)
				if id == "" {
					return fmt.Errorf("job %s has no machine (status %s)", args[0], j.Status)
				}
			}
			out, err := e.ec2().DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
			if err != nil || len(out.Reservations) == 0 {
				return fmt.Errorf("instance %s not found", id)
			}
			inst := out.Reservations[0].Instances[0]
			if inst.State.Name != ec2types.InstanceStateNameRunning {
				return fmt.Errorf("instance %s is %s", id, inst.State.Name)
			}
			if useSSH {
				ip := aws.ToString(inst.PublicIpAddress)
				if ip == "" {
					return fmt.Errorf("instance has no public IP; use SSM (without --ssh)")
				}
				a.ui.info("ssh runner@%s", ip)
				c := exec.Command("ssh", "runner@"+ip)
				c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
				return c.Run()
			}
			awsBin, err := exec.LookPath("aws")
			if err != nil {
				a.ui.println("Install the AWS CLI and the Session Manager plugin, then run:")
				a.ui.printf("  aws ssm start-session --region %s --target %s\n", e.cfg.Region, id)
				return nil
			}
			argv := []string{"ssm", "start-session", "--region", e.cfg.Region, "--target", id}
			if a.aws.Profile != "" {
				argv = append(argv, "--profile", a.aws.Profile)
			}
			c := exec.Command(awsBin, argv...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	}
	cmd.Flags().BoolVar(&useSSH, "ssh", false, "use SSH (needs the ssh=true label and an install with --ssh-cidr)")
	return cmd
}

// ---------------------------------------------------------------- quota

func (a *app) quotaCmd() *cobra.Command {
	var request int
	var gpu bool
	cmd := &cobra.Command{
		Use:   "quota",
		Short: "Show EC2 vCPU quotas and request increases",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, _, opts, err := awsInteractive(ctx, a.ui, a.aws, false)
			if err != nil {
				return err
			}
			a.aws = opts
			codes := [][2]string{{quotaSpotStandard, "Spot (standard families)"}, {quotaOnDemandStandard, "On-demand (standard families)"}, {quotaSpotGPU, "Spot (G and VT, GPU)"}, {quotaOnDemandGPU, "On-demand (G and VT, GPU)"}}
			var rows [][]string
			for _, c := range codes {
				v, err := quotaValue(ctx, cfg, c[0])
				s := fmt.Sprintf("%.0f vCPUs", v)
				if err != nil {
					s = "unknown"
				}
				rows = append(rows, []string{c[1], s, c[0]})
			}
			a.ui.table([]string{"QUOTA", "VALUE", "CODE"}, rows)
			if request == 0 {
				a.ui.printf("\nEach concurrent job uses its machine's vCPUs. Request more with: truner quota --request 512\n")
				return nil
			}
			sq := servicequotas.NewFromConfig(cfg)
			targets := []string{quotaSpotStandard, quotaOnDemandStandard}
			if gpu {
				targets = []string{quotaSpotGPU, quotaOnDemandGPU}
			}
			for _, code := range targets {
				_, err := sq.RequestServiceQuotaIncrease(ctx, &servicequotas.RequestServiceQuotaIncreaseInput{ServiceCode: aws.String("ec2"), QuotaCode: aws.String(code), DesiredValue: aws.Float64(float64(request))})
				if err != nil {
					a.ui.bad("%s: %v", code, err)
					continue
				}
				a.ui.ok("requested %d vCPUs for %s (AWS usually approves within minutes to a day)", request, code)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&request, "request", 0, "request this many vCPUs")
	cmd.Flags().BoolVar(&gpu, "gpu", false, "request GPU (G/VT) quotas instead")
	return cmd
}

// ---------------------------------------------------------------- github

func (a *app) githubCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "github", Short: "Inspect the GitHub App connection"}
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show the app, its installations and recent webhook deliveries",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			e, err := a.connect(ctx)
			if err != nil {
				return err
			}
			gh, err := e.github(ctx)
			if err != nil {
				return err
			}
			info, err := gh.GetApp(ctx)
			if err != nil {
				return err
			}
			a.ui.printf("%s %s\n", a.ui.bold(info.Name), a.ui.dim(info.HTMLURL))
			a.ui.printf("Install on more repositories: https://github.com/apps/%s/installations/new\n\n", info.Slug)
			ds, err := gh.ListHookDeliveries(ctx, 20)
			if err != nil {
				return err
			}
			var rows [][]string
			for _, d := range ds {
				t, _ := time.Parse(time.RFC3339, d.DeliveredAt)
				st := strconv.Itoa(d.StatusCode)
				if d.StatusCode >= 300 || d.StatusCode == 0 {
					st = a.ui.red(st)
				}
				rows = append(rows, []string{ago(t), d.Event + "." + d.Action, st, d.GUID})
			}
			a.ui.table([]string{"DELIVERED", "EVENT", "STATUS", "GUID"}, rows)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "install",
		Short: "Open the page to install the app on more repositories",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			e, err := a.connect(ctx)
			if err != nil {
				return err
			}
			gh, err := e.github(ctx)
			if err != nil {
				return err
			}
			url := "https://github.com/apps/" + gh.App().Slug + "/installations/new"
			a.ui.println(url)
			openBrowser(url)
			return nil
		},
	})
	return cmd
}
