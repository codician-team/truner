package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"

	"github.com/codician-team/truner/internal/config"
	"github.com/codician-team/truner/internal/controlplane"
	"github.com/codician-team/truner/internal/labels"
)

func (a *app) explainCmd() *cobra.Command {
	var repoCfg string
	var offline, asJSON bool
	cmd := &cobra.Command{
		Use:   "explain LABEL...",
		Short: "Show which machine a runs-on label resolves to (and what it costs)",
		Example: `  truner explain truner/cpu=8/ram=32
  truner explain truner runner=16cpu-linux-arm64 spot=false
  truner explain truner/runner=big --repo-config .github/truner.yml`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			u := a.ui
			prefix := firstNonEmpty(strings.SplitN(args[0], "/", 2)[0], "truner")
			prefix = strings.SplitN(prefix, "=", 2)[0]
			lbl, ok := labels.Parse(prefix, args)
			if !ok {
				return fmt.Errorf("labels must start with the truner label, e.g. truner/cpu=4")
			}
			var e *env
			g := &config.Global{}
			if !offline {
				var err error
				if e, err = a.connect(ctx); err == nil {
					if gl, err := loadGlobalConfig(ctx, e); err == nil {
						g = gl
					} else {
						u.warn("global config: %v", err)
					}
				} else {
					u.warn("%v (resolving with built-in defaults only)", err)
					e = nil
				}
			}
			var repo *config.Repo
			if repoCfg == "" {
				if _, err := os.Stat(".github/truner.yml"); err == nil {
					repoCfg = ".github/truner.yml"
				}
			}
			if repoCfg != "" {
				data, err := os.ReadFile(repoCfg)
				if err != nil {
					return err
				}
				if repo, err = config.ParseRepo(data); err != nil {
					return err
				}
			}
			spec, err := config.Resolver{Global: g, Repo: repo}.Resolve(lbl)
			if err != nil {
				return err
			}
			if asJSON {
				b, _ := json.MarshalIndent(spec, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			u.printf("%s %s\n\n", u.bold("Resolved:"), spec.Summary())
			row := func(k, v string) { u.printf("  %-14s %s\n", k, v) }
			if spec.Runner != "" {
				row("runner", spec.Runner)
			}
			if spec.Pool != "" {
				row("pool", spec.Pool)
			}
			if len(spec.Types) > 0 {
				row("types", strings.Join(spec.Types, ", "))
			} else {
				row("vCPU", spec.CPU.String())
				ram := spec.RAM.String() + " GiB"
				if spec.RAM.Min == 0 && spec.RAM.Max == 0 {
					ram = "any"
				}
				row("memory", ram)
				if len(spec.Families) > 0 {
					row("families", strings.Join(spec.Families, ", "))
				}
			}
			row("arch", spec.Arch)
			if spec.GPU > 0 {
				row("GPU", strconv.Itoa(spec.GPU))
			}
			row("image", fmt.Sprintf("%s (%s, profile %s)", spec.Image, spec.Platform(), firstNonEmpty(spec.ImageSpec.Profile, "none")))
			purchase := map[config.SpotMode]string{config.SpotAuto: "spot, falls back to on-demand", config.SpotOnly: "spot only", config.SpotOff: "on-demand"}[spec.Spot]
			row("purchase", purchase)
			disk := fmt.Sprintf("%d GiB gp3", spec.Disk)
			if spec.DiskIOPS > 0 || spec.DiskThroughput > 0 {
				disk += fmt.Sprintf(" (%d IOPS, %d MiB/s)", max(spec.DiskIOPS, 3000), max(spec.DiskThroughput, 125))
			}
			if spec.NVMe {
				disk += " + local NVMe required"
			}
			row("disk", disk)
			row("cache", spec.Cache)
			row("timeout", fmt.Sprintf("%d min (idle %d min)", spec.Timeout, spec.IdleTimeout))
			if spec.Private {
				row("network", "private subnets (static egress IP)")
			}
			if spec.SSH {
				row("ssh", "enabled")
			}
			if spec.Keep > 0 {
				row("keep", fmt.Sprintf("%d min after the job", spec.Keep))
			}
			for _, w := range spec.Warnings {
				u.warn("%s", w)
			}
			if e != nil {
				u.println()
				candidates(ctx, u, e, spec)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoCfg, "repo-config", "", "repository config to apply (default .github/truner.yml if present)")
	cmd.Flags().BoolVar(&offline, "offline", false, "do not contact AWS")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the resolved spec as JSON")
	return cmd
}

func loadGlobalConfig(ctx context.Context, e *env) (*config.Global, error) {
	out, err := e.s3().GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(e.out("DataBucket")), Key: aws.String(controlplane.GlobalConfigKey)})
	if err != nil {
		if strings.Contains(err.Error(), "NoSuchKey") {
			return &config.Global{}, nil
		}
		return nil, err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, err
	}
	return config.ParseGlobal(data)
}

// candidates lists instance types EC2 would consider, with current spot prices.
func candidates(ctx context.Context, u *ui, e *env, spec *config.JobSpec) {
	c := e.ec2()
	var types []string
	if len(spec.Types) > 0 {
		types = spec.Types
	} else {
		req := controlplane.InstanceRequirements(spec)
		arch := ec2types.ArchitectureTypeX8664
		if spec.Arch == config.ArchARM64 {
			arch = ec2types.ArchitectureTypeArm64
		}
		in := &ec2.GetInstanceTypesFromInstanceRequirementsInput{
			ArchitectureTypes:    []ec2types.ArchitectureType{arch},
			VirtualizationTypes:  []ec2types.VirtualizationType{ec2types.VirtualizationTypeHvm},
			InstanceRequirements: req,
		}
		p := ec2.NewGetInstanceTypesFromInstanceRequirementsPaginator(c, in)
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				u.warn("could not list matching instance types: %v", err)
				return
			}
			for _, t := range page.InstanceTypes {
				types = append(types, aws.ToString(t.InstanceType))
			}
		}
	}
	if len(types) == 0 {
		u.bad("no instance type in %s matches this spec", e.cfg.Region)
		return
	}
	desc := "Linux/UNIX"
	if spec.Platform() == config.PlatformWindows {
		desc = "Windows"
	}
	prices := map[string]float64{}
	var its []ec2types.InstanceType
	for _, t := range types {
		its = append(its, ec2types.InstanceType(t))
	}
	for i := 0; i < len(its); i += 100 {
		j := min(i+100, len(its))
		p := ec2.NewDescribeSpotPriceHistoryPaginator(c, &ec2.DescribeSpotPriceHistoryInput{InstanceTypes: its[i:j], ProductDescriptions: []string{desc}, StartTime: aws.Time(time.Now())})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				break
			}
			for _, h := range page.SpotPriceHistory {
				t := string(h.InstanceType)
				v, _ := strconv.ParseFloat(aws.ToString(h.SpotPrice), 64)
				if cur, ok := prices[t]; !ok || v < cur {
					prices[t] = v
				}
			}
		}
	}
	sort.Slice(types, func(i, j int) bool {
		pi, oki := prices[types[i]]
		pj, okj := prices[types[j]]
		if oki != okj {
			return oki
		}
		return pi < pj
	})
	u.printf("%s %d instance types match in %s; cheapest current spot prices:\n", u.bold("Candidates:"), len(types), e.cfg.Region)
	var rows [][]string
	for i, t := range types {
		if i == 12 {
			break
		}
		p := "-"
		if v, ok := prices[t]; ok {
			p = fmt.Sprintf("$%.4f/h", v)
		}
		rows = append(rows, []string{"  " + t, p})
	}
	u.table([]string{"  TYPE", "SPOT (lowest AZ)"}, rows)
	u.printf("%s\n", u.dim("EC2 Fleet picks among all candidates and AZs with the price-capacity-optimized strategy."))
}
