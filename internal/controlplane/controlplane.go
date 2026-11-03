package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/truner/truner/internal/config"
	"github.com/truner/truner/internal/github"
)

// GlobalConfigKey is where the account-wide config lives in the data bucket.
const GlobalConfigKey = "config/truner.yml"

// ControlPlane bundles the clients and caches shared by all handlers.
type ControlPlane struct {
	Env     *Env
	Log     *slog.Logger
	AWS     aws.Config
	EC2     *ec2.Client
	S3      *s3.Client
	Presign *s3.PresignClient
	SQS     *sqs.Client
	SSM     *ssm.Client
	SNS     *sns.Client
	Pricing *pricing.Client
	Store   *Store

	mu         sync.Mutex
	gh         *github.Client
	ghLoaded   time.Time
	global     *config.Global
	globalErr  error
	globalAt   time.Time
	repoCfg    map[string]cachedRepoCfg
	images     map[string]cachedImage
	priceCache map[string]cachedPrice
	runnerRel  *runnerRelease
	runnerAt   time.Time
	count      int
	countAt    time.Time
}

type cachedRepoCfg struct {
	cfg *config.Repo
	err error
	at  time.Time
}

// New builds a control plane from the environment.
func New(ctx context.Context) (*ControlPlane, error) {
	env, err := LoadEnv()
	if err != nil {
		return nil, err
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(env.Region))
	if err != nil {
		return nil, err
	}
	return NewWithConfig(env, cfg), nil
}

// NewWithConfig builds a control plane with explicit settings.
func NewWithConfig(env *Env, cfg aws.Config) *ControlPlane {
	s3c := s3.NewFromConfig(cfg, func(o *s3.Options) {
		// Path-style addressing for S3-compatible endpoints and tests.
		o.UsePathStyle = os.Getenv("TRUNER_S3_PATH_STYLE") == "true"
	})
	ddb := dynamodb.NewFromConfig(cfg)
	pcfg := cfg.Copy()
	pcfg.Region = "us-east-1" // the Pricing API lives in us-east-1
	return &ControlPlane{
		Env:        env,
		Log:        slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
		AWS:        cfg,
		EC2:        ec2.NewFromConfig(cfg),
		S3:         s3c,
		Presign:    s3.NewPresignClient(s3c),
		SQS:        sqs.NewFromConfig(cfg),
		SSM:        ssm.NewFromConfig(cfg),
		SNS:        sns.NewFromConfig(cfg),
		Pricing:    pricing.NewFromConfig(pcfg),
		Store:      &Store{DB: ddb, Table: env.Table},
		repoCfg:    map[string]cachedRepoCfg{},
		images:     map[string]cachedImage{},
		priceCache: map[string]cachedPrice{},
	}
}

// ErrNoApp means the GitHub App has not been configured yet.
var ErrNoApp = errors.New("GitHub App is not configured (run `truner install`)")

// GitHub returns a GitHub App client, reloading credentials every few minutes
// so a rotated key is picked up without a redeploy.
func (cp *ControlPlane) GitHub(ctx context.Context) (*github.Client, error) {
	cp.mu.Lock()
	if cp.gh != nil && time.Since(cp.ghLoaded) < 5*time.Minute {
		gh := cp.gh
		cp.mu.Unlock()
		return gh, nil
	}
	cp.mu.Unlock()
	out, err := cp.SSM.GetParameter(ctx, &ssm.GetParameterInput{Name: &cp.Env.AppParam, WithDecryption: aws.Bool(true)})
	if err != nil {
		if strings.Contains(err.Error(), "ParameterNotFound") {
			return nil, ErrNoApp
		}
		return nil, fmt.Errorf("read GitHub App credentials: %w", err)
	}
	var creds github.AppCredentials
	if err := json.Unmarshal([]byte(aws.ToString(out.Parameter.Value)), &creds); err != nil || creds.AppID == 0 {
		return nil, ErrNoApp
	}
	gh, err := github.New(&creds)
	if err != nil {
		return nil, err
	}
	cp.mu.Lock()
	// Keep the old client (and its token cache) if the credentials did not change.
	if cp.gh != nil && cp.gh.App().AppID == creds.AppID && cp.gh.App().PrivateKeyPEM == creds.PrivateKeyPEM {
		gh = cp.gh
	}
	cp.gh, cp.ghLoaded = gh, time.Now()
	cp.mu.Unlock()
	return gh, nil
}

// GlobalConfig returns the account-wide config (cached for a minute). A
// broken config file never blocks launches: the last good version (or the
// defaults) is used and the error is logged.
func (cp *ControlPlane) GlobalConfig(ctx context.Context) *config.Global {
	cp.mu.Lock()
	if cp.global != nil && time.Since(cp.globalAt) < time.Minute {
		g := cp.global
		cp.mu.Unlock()
		return g
	}
	cp.mu.Unlock()
	g, err := cp.loadGlobal(ctx)
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if err != nil {
		cp.Log.Error("invalid global config, using last good version", "err", err)
		cp.globalErr = err
		if cp.global == nil {
			cp.global = &config.Global{}
		}
		cp.globalAt = time.Now()
		return cp.global
	}
	cp.global, cp.globalErr, cp.globalAt = g, nil, time.Now()
	return g
}

func (cp *ControlPlane) loadGlobal(ctx context.Context) (*config.Global, error) {
	out, err := cp.S3.GetObject(ctx, &s3.GetObjectInput{Bucket: &cp.Env.Bucket, Key: aws.String(GlobalConfigKey)})
	if err != nil {
		if strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), "StatusCode: 404") {
			return &config.Global{}, nil
		}
		return nil, err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(io.LimitReader(out.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return config.ParseGlobal(data)
}

// RepoConfig loads .github/truner.yml for a commit (cached for 5 minutes).
func (cp *ControlPlane) RepoConfig(ctx context.Context, gh *github.Client, j *Job) (*config.Repo, error) {
	ref := j.SHA
	key := j.FullName() + "@" + ref
	cp.mu.Lock()
	if c, ok := cp.repoCfg[key]; ok && time.Since(c.at) < 5*time.Minute {
		cp.mu.Unlock()
		return c.cfg, c.err
	}
	cp.mu.Unlock()
	var data []byte
	var err error
	for _, p := range []string{".github/truner.yml", ".github/truner.yaml"} {
		data, err = gh.GetFile(ctx, j.InstallationID, j.Owner, j.Repo, p, ref)
		if err != nil || data != nil {
			break
		}
	}
	var cfg *config.Repo
	switch {
	case err != nil:
		// Missing "contents" permission or a transient error: proceed without repo config.
		cp.Log.Warn("could not read repo config", "repo", j.FullName(), "err", err)
		cfg, err = &config.Repo{}, nil
	case data == nil:
		cfg = &config.Repo{}
	default:
		cfg, err = config.ParseRepo(data)
		if err != nil {
			err = fmt.Errorf(".github/truner.yml: %w", err)
		}
	}
	cp.mu.Lock()
	if len(cp.repoCfg) > 500 {
		cp.repoCfg = map[string]cachedRepoCfg{}
	}
	cp.repoCfg[key] = cachedRepoCfg{cfg: cfg, err: err, at: time.Now()}
	cp.mu.Unlock()
	return cfg, err
}

// alert publishes a message to the alert topic, if configured.
func (cp *ControlPlane) alert(ctx context.Context, subject, msg string) {
	if cp.Env.AlertTopicArn == "" {
		return
	}
	if len(subject) > 99 {
		subject = subject[:99]
	}
	_, err := cp.SNS.Publish(ctx, &sns.PublishInput{TopicArn: &cp.Env.AlertTopicArn, Subject: aws.String(subject), Message: aws.String(msg)})
	if err != nil {
		cp.Log.Error("alert publish failed", "err", err)
	}
}
