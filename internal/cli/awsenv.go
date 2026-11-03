package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// state remembers the installation the CLI talks to by default.
type state struct {
	Stack   string `json:"stack,omitempty"`
	Region  string `json:"region,omitempty"`
	Profile string `json:"profile,omitempty"`
	// Setups are GitHub App setup links still waiting for the user, by "region/stack".
	Setups map[string]pendingSetup `json:"setups,omitempty"`
}

type pendingSetup struct {
	State   string `json:"state"`
	Owner   string `json:"owner,omitempty"`
	Replace bool   `json:"replace,omitempty"`
}

func statePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".truner", "config.json")
}

func loadState() state {
	var s state
	if data, err := os.ReadFile(statePath()); err == nil {
		json.Unmarshal(data, &s)
	}
	return s
}

func saveState(s state) error {
	p := statePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(p, data, 0o600)
}

// awsOptions are the user's AWS choices from flags, env and saved state.
type awsOptions struct {
	Profile string
	Region  string
}

func (o awsOptions) resolve() awsOptions {
	st := loadState()
	if o.Profile == "" && os.Getenv("AWS_PROFILE") == "" && os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		o.Profile = st.Profile
	}
	if o.Region == "" {
		o.Region = firstNonEmpty(os.Getenv("AWS_REGION"), os.Getenv("AWS_DEFAULT_REGION"), st.Region)
	}
	return o
}

func loadAWS(ctx context.Context, o awsOptions) (aws.Config, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if o.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(o.Profile))
	}
	if o.Region != "" {
		opts = append(opts, awsconfig.WithRegion(o.Region))
	}
	return awsconfig.LoadDefaultConfig(ctx, opts...)
}

type identity struct {
	Account, Arn string
}

func whoami(ctx context.Context, cfg aws.Config) (*identity, error) {
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, err
	}
	return &identity{Account: aws.ToString(out.Account), Arn: aws.ToString(out.Arn)}, nil
}

var regionRe = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z]+-\d$`)

// awsInteractive returns working AWS credentials, asking for an access key
// when none are configured. With save, entered keys become the "truner" profile.
func awsInteractive(ctx context.Context, u *ui, o awsOptions, askRegion bool) (aws.Config, *identity, awsOptions, error) {
	o = o.resolve()
	cfg, err := loadAWS(ctx, o)
	if err == nil {
		if o.Region == "" && cfg.Region == "" || askRegion && o.Region == "" {
			def := firstNonEmpty(cfg.Region, "us-east-1")
			o.Region = u.ask("AWS region", def)
			cfg.Region = o.Region
		}
		if cfg.Region == "" {
			cfg.Region = "us-east-1"
		}
		if id, err := whoami(ctx, cfg); err == nil {
			return cfg, id, o, nil
		} else if !u.tty || u.yes {
			if strings.Contains(err.Error(), "get credentials") {
				return aws.Config{}, nil, o, errors.New("no AWS credentials found.\n" + loginHint)
			}
			return aws.Config{}, nil, o, fmt.Errorf("AWS credentials are not working (%w).\n%s", err, loginHint)
		}
	}
	if !u.tty {
		return aws.Config{}, nil, o, errors.New("no AWS credentials found.\n" + loginHint)
	}
	u.println()
	u.info("No working AWS credentials were found.")
	return promptAccessKey(ctx, u, o, false)
}

// loginHint is printed when a non-interactive run (a script or an AI agent)
// has no AWS credentials. The secret key should be typed by the user, never
// passed through the agent.
const loginHint = "ACTION REQUIRED: run `truner login` in your own terminal to enter an AWS access key\n" +
	"(or set up the AWS CLI with `aws configure` or `aws sso login`), then run this command again."

// promptAccessKey asks for an access key until one works. With save the key
// is stored as the "truner" profile without asking.
func promptAccessKey(ctx context.Context, u *ui, o awsOptions, save bool) (aws.Config, *identity, awsOptions, error) {
	u.info("Paste an access key of an IAM user with administrator access to the account")
	u.info("truner should run in.")
	u.info("%s", u.dim("(Create one in the AWS console: IAM > Users > Create user > Security credentials.)"))
	for attempt := 0; attempt < 3; attempt++ {
		keyID := u.ask("AWS Access Key ID", "")
		secret := u.secret("AWS Secret Access Key")
		token := ""
		if strings.HasPrefix(keyID, "ASIA") {
			token = u.secret("AWS Session Token")
		}
		region := o.Region
		if region == "" {
			region = u.ask("AWS region", "us-east-1")
		}
		if !regionRe.MatchString(region) {
			u.bad("%q does not look like an AWS region", region)
			continue
		}
		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region),
			awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(keyID, secret, token)))
		if err != nil {
			return aws.Config{}, nil, o, err
		}
		id, err := whoami(ctx, cfg)
		if err != nil {
			u.bad("those credentials did not work: %v", err)
			continue
		}
		o.Region = region
		if save || u.confirm("Save these credentials as AWS profile \"truner\" for future commands?", true) {
			if err := saveProfile("truner", keyID, secret, token, region); err != nil {
				u.warn("could not save the profile: %v", err)
			} else {
				o.Profile = "truner"
				u.ok("Saved to %s (profile \"truner\")", awsCredentialsPath())
			}
		}
		return cfg, id, o, nil
	}
	return aws.Config{}, nil, o, errors.New("no working AWS credentials")
}

func awsCredentialsPath() string {
	if p := os.Getenv("AWS_SHARED_CREDENTIALS_FILE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aws", "credentials")
}

func awsConfigPath() string {
	if p := os.Getenv("AWS_CONFIG_FILE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aws", "config")
}

func saveProfile(name, keyID, secret, token, region string) error {
	kv := [][2]string{{"aws_access_key_id", keyID}, {"aws_secret_access_key", secret}}
	if token != "" {
		kv = append(kv, [2]string{"aws_session_token", token})
	}
	if err := upsertINISection(awsCredentialsPath(), name, kv); err != nil {
		return err
	}
	return upsertINISection(awsConfigPath(), "profile "+name, [][2]string{{"region", region}})
}

// upsertINISection replaces (or appends) one section of an INI file.
func upsertINISection(path, section string, kv [][2]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	var out []string
	skip := false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			skip = strings.TrimSpace(t[1:len(t)-1]) == section
		}
		if !skip {
			out = append(out, l)
		}
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	if len(out) > 0 {
		out = append(out, "")
	}
	out = append(out, "["+section+"]")
	for _, p := range kv {
		out = append(out, p[0]+" = "+p[1])
	}
	out = append(out, "")
	return os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o600)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
