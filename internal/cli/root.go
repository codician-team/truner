// Package cli implements the `truner` command line tool.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/spf13/cobra"

	"github.com/codician-team/truner/internal/controlplane"
	"github.com/codician-team/truner/internal/deploy"
	"github.com/codician-team/truner/internal/github"
	"github.com/codician-team/truner/internal/version"
)

type app struct {
	ui    *ui
	aws   awsOptions
	stack string
	yes   bool
}

// Execute runs the CLI.
func Execute() int {
	a := &app{}
	root := &cobra.Command{
		Use:   "truner",
		Short: "Ephemeral, autoscaling GitHub Actions runners in your own AWS account",
		Long: `truner runs every GitHub Actions job on a fresh EC2 machine in your AWS
account: spot pricing with on-demand fallback, x64/arm64/GPU/Windows, an
S3-backed cache, warm pools and zero idle cost.

Get started:  truner install`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			a.ui = newUI(a.yes)
			if a.stack == "" {
				a.stack = firstNonEmpty(os.Getenv("TRUNER_STACK"), loadState().Stack, "truner")
			}
		},
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.PersistentFlags().StringVar(&a.stack, "stack", "", "installation (CloudFormation stack) name (default \"truner\")")
	root.PersistentFlags().StringVar(&a.aws.Region, "region", "", "AWS region")
	root.PersistentFlags().StringVar(&a.aws.Profile, "profile", "", "AWS profile")
	root.PersistentFlags().BoolVarP(&a.yes, "yes", "y", false, "assume yes; never prompt")

	root.AddCommand(
		a.installCmd(), a.upgradeCmd(), a.uninstallCmd(),
		a.statusCmd(), a.doctorCmd(),
		a.jobsCmd(), a.costsCmd(), a.logsCmd(), a.explainCmd(),
		a.imagesCmd(), a.cacheCmd(), a.configCmd(), a.poolsCmd(),
		a.connectCmd(), a.quotaCmd(), a.githubCmd(), a.loginCmd(), a.versionCmd(),
	)
	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}
	return 0
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run:   func(cmd *cobra.Command, args []string) { fmt.Println(version.String()) },
	}
}

// env is a connected installation.
type env struct {
	cfg   aws.Config
	name  string
	stack *deploy.Stack
}

func (e *env) out(k string) string { return e.stack.Outputs[k] }

func (e *env) ec2() *ec2.Client             { return ec2.NewFromConfig(e.cfg) }
func (e *env) s3() *s3.Client               { return s3.NewFromConfig(e.cfg) }
func (e *env) ddb() *dynamodb.Client        { return dynamodb.NewFromConfig(e.cfg) }
func (e *env) lambdaClient() *lambda.Client { return lambda.NewFromConfig(e.cfg) }
func (e *env) store() *controlplane.Store {
	return &controlplane.Store{DB: e.ddb(), Table: e.out("Table")}
}

var errNotInstalled = errors.New("truner is not installed in this account/region (run `truner install`, or pass --stack/--region)")

// connect loads AWS credentials and the installed stack.
func (a *app) connect(ctx context.Context) (*env, error) {
	cfg, _, opts, err := awsInteractive(ctx, a.ui, a.aws, false)
	if err != nil {
		return nil, err
	}
	a.aws = opts
	st, err := deploy.Describe(ctx, cfg, a.stack)
	if errors.Is(err, deploy.ErrNoStack) {
		return nil, errNotInstalled
	}
	if err != nil {
		return nil, err
	}
	// On stderr, so that output such as `explain --json` stays parseable.
	if older, msg := cliSkew(st.Outputs["Version"]); older {
		fmt.Fprintf(os.Stderr, "%s %s\n\n", a.ui.yellow("!"), msg)
	}
	return &env{cfg: cfg, name: a.stack, stack: st}, nil
}

// github returns a GitHub App client from the credentials stored in SSM.
func (e *env) github(ctx context.Context) (*github.Client, error) {
	name := e.out("AppParameter")
	if name == "" {
		name = "/truner/" + e.name + "/github-app"
	}
	out, err := ssm.NewFromConfig(e.cfg).GetParameter(ctx, &ssm.GetParameterInput{Name: &name, WithDecryption: aws.Bool(true)})
	if err != nil {
		if strings.Contains(err.Error(), "ParameterNotFound") {
			return nil, controlplane.ErrNoApp
		}
		return nil, err
	}
	var creds github.AppCredentials
	if err := json.Unmarshal([]byte(aws.ToString(out.Parameter.Value)), &creds); err != nil {
		return nil, err
	}
	return github.New(&creds)
}

// invoke calls the janitor function with an admin request.
func (e *env) invoke(ctx context.Context, req controlplane.AdminRequest, async bool) (*controlplane.AdminResponse, error) {
	payload, _ := json.Marshal(req)
	in := &lambda.InvokeInput{FunctionName: aws.String(e.out("JanitorFunction")), Payload: payload}
	if async {
		in.InvocationType = lambdatypes.InvocationTypeEvent
	}
	ctx, cancel := context.WithTimeout(ctx, 11*time.Minute)
	defer cancel()
	out, err := e.lambdaClient().Invoke(ctx, in)
	if err != nil {
		return nil, err
	}
	if async {
		return &controlplane.AdminResponse{OK: true}, nil
	}
	if out.FunctionError != nil {
		return nil, fmt.Errorf("control plane error: %s", string(out.Payload))
	}
	var resp controlplane.AdminResponse
	if err := json.Unmarshal(out.Payload, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
