// Package controlplane implements truner's serverless control plane: the
// webhook receiver, the launch worker and the janitor. All three run from
// the same Lambda binary.
package controlplane

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/truner/truner/internal/labels"
)

// Env is the control plane configuration, read from Lambda environment
// variables set by the CloudFormation stack.
type Env struct {
	Stack              string
	Region             string
	Bucket             string // data bucket: cache, logs, config, runner mirror
	ArtifactsBucket    string // agent binaries
	Table              string
	QueueURL           string
	AppParam           string // SSM parameter holding the GitHub App credentials
	PublicSubnets      []string
	PrivateSubnets     []string
	SecurityGroup      string
	InstanceProfileArn string
	ECRRepoURI         string
	LabelPrefix        string
	AlertTopicArn      string
	Version            string // agent version to run on instances
	SSHEnabled         bool
	DailyReport        bool
	JanitorFunction    string
}

// LoadEnv reads the environment.
func LoadEnv() (*Env, error) {
	e := &Env{
		Stack:              os.Getenv("TRUNER_STACK"),
		Region:             firstNonEmpty(os.Getenv("TRUNER_REGION"), os.Getenv("AWS_REGION")),
		Bucket:             os.Getenv("TRUNER_BUCKET"),
		ArtifactsBucket:    os.Getenv("TRUNER_ARTIFACTS_BUCKET"),
		Table:              os.Getenv("TRUNER_TABLE"),
		QueueURL:           os.Getenv("TRUNER_QUEUE_URL"),
		AppParam:           os.Getenv("TRUNER_APP_PARAM"),
		PublicSubnets:      splitCSV(os.Getenv("TRUNER_PUBLIC_SUBNETS")),
		PrivateSubnets:     splitCSV(os.Getenv("TRUNER_PRIVATE_SUBNETS")),
		SecurityGroup:      os.Getenv("TRUNER_SECURITY_GROUP"),
		InstanceProfileArn: os.Getenv("TRUNER_INSTANCE_PROFILE_ARN"),
		ECRRepoURI:         os.Getenv("TRUNER_ECR_REPO_URI"),
		LabelPrefix:        firstNonEmpty(os.Getenv("TRUNER_LABEL_PREFIX"), labels.DefaultPrefix),
		AlertTopicArn:      os.Getenv("TRUNER_ALERT_TOPIC_ARN"),
		Version:            os.Getenv("TRUNER_VERSION"),
		JanitorFunction:    os.Getenv("TRUNER_JANITOR_FUNCTION"),
	}
	e.SSHEnabled, _ = strconv.ParseBool(os.Getenv("TRUNER_SSH_ENABLED"))
	e.DailyReport, _ = strconv.ParseBool(os.Getenv("TRUNER_DAILY_REPORT"))
	var missing []string
	for k, v := range map[string]string{"TRUNER_STACK": e.Stack, "TRUNER_BUCKET": e.Bucket, "TRUNER_TABLE": e.Table, "TRUNER_QUEUE_URL": e.QueueURL, "TRUNER_APP_PARAM": e.AppParam, "AWS_REGION": e.Region} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing environment: %s", strings.Join(missing, ", "))
	}
	return e, nil
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
