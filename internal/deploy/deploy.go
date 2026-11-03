// Package deploy installs, upgrades and removes the truner CloudFormation stack.
package deploy

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Template is the CloudFormation template of a truner installation.
//
//go:embed template.yaml
var Template string

// ArtifactsBucketName is the per-account, per-region bucket for deploy artifacts.
func ArtifactsBucketName(account, region string) string {
	return fmt.Sprintf("truner-artifacts-%s-%s", account, region)
}

// AccountID returns the caller's account id.
func AccountID(ctx context.Context, cfg aws.Config) (string, error) {
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.Account), nil
}

// EnsureBucket creates a private bucket if it does not exist yet.
func EnsureBucket(ctx context.Context, cfg aws.Config, bucket string) error {
	c := s3.NewFromConfig(cfg)
	if _, err := c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bucket}); err == nil {
		return nil
	}
	in := &s3.CreateBucketInput{Bucket: &bucket}
	if cfg.Region != "us-east-1" {
		in.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{LocationConstraint: s3types.BucketLocationConstraint(cfg.Region)}
	}
	if _, err := c.CreateBucket(ctx, in); err != nil {
		var owned *s3types.BucketAlreadyOwnedByYou
		if !errors.As(err, &owned) {
			return fmt.Errorf("create bucket %s: %w", bucket, err)
		}
	}
	_, err := c.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket: &bucket,
		PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
			BlockPublicAcls: aws.Bool(true), BlockPublicPolicy: aws.Bool(true), IgnorePublicAcls: aws.Bool(true), RestrictPublicBuckets: aws.Bool(true),
		},
	})
	if err != nil {
		return err
	}
	_, err = c.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: &bucket, Tagging: &s3types.Tagging{TagSet: []s3types.Tag{{Key: aws.String("truner:managed"), Value: aws.String("true")}}}})
	return err
}

// Upload stores an object.
func Upload(ctx context.Context, cfg aws.Config, bucket, key string, data []byte) error {
	_, err := s3.NewFromConfig(cfg).PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: &key, Body: bytes.NewReader(data)})
	if err != nil {
		return fmt.Errorf("upload s3://%s/%s: %w", bucket, key, err)
	}
	return nil
}

// Stack is the state of a deployed stack.
type Stack struct {
	Name       string
	Status     string
	Reason     string
	Outputs    map[string]string
	Parameters map[string]string
	Updated    time.Time
}

// ErrNoStack means the stack does not exist.
var ErrNoStack = errors.New("stack not found")

// Describe loads a stack.
func Describe(ctx context.Context, cfg aws.Config, name string) (*Stack, error) {
	out, err := cloudformation.NewFromConfig(cfg).DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: &name})
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return nil, ErrNoStack
		}
		return nil, err
	}
	if len(out.Stacks) == 0 {
		return nil, ErrNoStack
	}
	s := out.Stacks[0]
	st := &Stack{Name: name, Status: string(s.StackStatus), Reason: aws.ToString(s.StackStatusReason), Outputs: map[string]string{}, Parameters: map[string]string{}}
	for _, o := range s.Outputs {
		st.Outputs[aws.ToString(o.OutputKey)] = aws.ToString(o.OutputValue)
	}
	for _, p := range s.Parameters {
		st.Parameters[aws.ToString(p.ParameterKey)] = aws.ToString(p.ParameterValue)
	}
	if s.LastUpdatedTime != nil {
		st.Updated = *s.LastUpdatedTime
	} else if s.CreationTime != nil {
		st.Updated = *s.CreationTime
	}
	return st, nil
}

// templateParameters lists the parameter names declared by the template.
func templateParameters() []string {
	var out []string
	in := false
	for _, l := range strings.Split(Template, "\n") {
		if strings.HasPrefix(l, "Parameters:") {
			in = true
			continue
		}
		if in && len(l) > 0 && l[0] != ' ' {
			break
		}
		if in && strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ") && strings.HasSuffix(strings.TrimSpace(l), ":") {
			out = append(out, strings.TrimSuffix(strings.TrimSpace(l), ":"))
		}
	}
	return out
}

// Deploy creates or updates the stack and streams progress to w. Parameters
// not given keep their previous value on update (or the template default).
func Deploy(ctx context.Context, cfg aws.Config, name, templateURL string, params map[string]string, w io.Writer) (*Stack, error) {
	cf := cloudformation.NewFromConfig(cfg)
	cur, err := Describe(ctx, cfg, name)
	if err != nil && !errors.Is(err, ErrNoStack) {
		return nil, err
	}
	// An interrupted earlier run (Ctrl-C, or an AI agent's command timeout)
	// can leave an operation running: wait for it, then look again, because
	// a failed create deletes the stack.
	if cur != nil && strings.HasSuffix(cur.Status, "_IN_PROGRESS") {
		fmt.Fprintf(w, "Waiting for the current stack operation (%s) to finish...\n", cur.Status)
		if _, err := wait(ctx, cf, name, time.Now().Add(-time.Minute), w); err != nil {
			return nil, err
		}
		if cur, err = Describe(ctx, cfg, name); err != nil && !errors.Is(err, ErrNoStack) {
			return nil, err
		}
	}
	if cur != nil && (cur.Status == "ROLLBACK_COMPLETE" || cur.Status == "DELETE_COMPLETE") {
		fmt.Fprintf(w, "Removing failed previous attempt (%s)...\n", cur.Status)
		if err := Delete(ctx, cfg, name, w); err != nil {
			return nil, err
		}
		cur = nil
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var cfParams []cftypes.Parameter
	for _, k := range keys {
		cfParams = append(cfParams, cftypes.Parameter{ParameterKey: aws.String(k), ParameterValue: aws.String(params[k])})
	}
	tags := []cftypes.Tag{{Key: aws.String("truner:managed"), Value: aws.String("true")}, {Key: aws.String("truner:stack"), Value: aws.String(name)}}
	caps := []cftypes.Capability{cftypes.CapabilityCapabilityIam, cftypes.CapabilityCapabilityNamedIam}
	started := time.Now().Add(-5 * time.Second)
	// Track the stack by id: after a failed create (OnFailure: DELETE) the
	// name no longer resolves, but the id still shows what went wrong.
	stackID := name
	if cur == nil {
		var out *cloudformation.CreateStackOutput
		out, err = cf.CreateStack(ctx, &cloudformation.CreateStackInput{
			StackName: &name, TemplateURL: &templateURL, Parameters: cfParams, Capabilities: caps, Tags: tags,
			OnFailure: cftypes.OnFailureDelete, EnableTerminationProtection: aws.Bool(false),
		})
		if err == nil {
			stackID = aws.ToString(out.StackId)
		}
	} else {
		for _, p := range templateParameters() {
			if _, ok := params[p]; !ok {
				if _, had := cur.Parameters[p]; had {
					cfParams = append(cfParams, cftypes.Parameter{ParameterKey: aws.String(p), UsePreviousValue: aws.Bool(true)})
				}
			}
		}
		var out *cloudformation.UpdateStackOutput
		out, err = cf.UpdateStack(ctx, &cloudformation.UpdateStackInput{
			StackName: &name, TemplateURL: &templateURL, Parameters: cfParams, Capabilities: caps, Tags: tags,
		})
		if err == nil {
			stackID = aws.ToString(out.StackId)
		}
		if err != nil && strings.Contains(err.Error(), "No updates are to be performed") {
			fmt.Fprintln(w, "Stack is already up to date.")
			return Describe(ctx, cfg, name)
		}
	}
	if err != nil {
		return nil, err
	}
	status, err := wait(ctx, cf, stackID, started, w)
	if err != nil {
		return nil, err
	}
	if status != "CREATE_COMPLETE" && status != "UPDATE_COMPLETE" {
		reason := failureReason(ctx, cf, stackID, started)
		return nil, fmt.Errorf("stack ended in %s: %s", status, reason)
	}
	return Describe(ctx, cfg, name)
}

// Delete removes the stack and waits for it.
func Delete(ctx context.Context, cfg aws.Config, name string, w io.Writer) error {
	cf := cloudformation.NewFromConfig(cfg)
	started := time.Now().Add(-5 * time.Second)
	if _, err := cf.DeleteStack(ctx, &cloudformation.DeleteStackInput{StackName: &name}); err != nil {
		return err
	}
	status, err := wait(ctx, cf, name, started, w)
	if err != nil {
		if errors.Is(err, ErrNoStack) {
			return nil
		}
		return err
	}
	if status != "DELETE_COMPLETE" {
		return fmt.Errorf("stack deletion ended in %s: %s", status, failureReason(ctx, cf, name, started))
	}
	return nil
}

// wait polls until the stack reaches a terminal state, printing resource events.
func wait(ctx context.Context, cf *cloudformation.Client, name string, since time.Time, w io.Writer) (string, error) {
	seen := map[string]bool{}
	for {
		out, err := cf.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: &name})
		if err != nil {
			if strings.Contains(err.Error(), "does not exist") {
				return "DELETE_COMPLETE", nil
			}
			return "", err
		}
		status := string(out.Stacks[0].StackStatus)
		if ev, err := cf.DescribeStackEvents(ctx, &cloudformation.DescribeStackEventsInput{StackName: out.Stacks[0].StackId}); err == nil {
			var fresh []cftypes.StackEvent
			for _, e := range ev.StackEvents {
				id := aws.ToString(e.EventId)
				if seen[id] || e.Timestamp == nil || e.Timestamp.Before(since) {
					continue
				}
				seen[id] = true
				fresh = append(fresh, e)
			}
			for i := len(fresh) - 1; i >= 0; i-- {
				e := fresh[i]
				line := fmt.Sprintf("  %-38s %-20s %s", truncate(aws.ToString(e.LogicalResourceId), 38), e.ResourceStatus, aws.ToString(e.ResourceStatusReason))
				fmt.Fprintln(w, strings.TrimRight(line, " "))
			}
		}
		if !strings.HasSuffix(status, "_IN_PROGRESS") {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(4 * time.Second):
		}
	}
}

func failureReason(ctx context.Context, cf *cloudformation.Client, name string, since time.Time) string {
	ev, err := cf.DescribeStackEvents(ctx, &cloudformation.DescribeStackEventsInput{StackName: &name})
	if err != nil {
		return "see the CloudFormation console"
	}
	for i := len(ev.StackEvents) - 1; i >= 0; i-- {
		e := ev.StackEvents[i]
		if e.Timestamp != nil && e.Timestamp.After(since) && strings.HasSuffix(string(e.ResourceStatus), "_FAILED") {
			if r := aws.ToString(e.ResourceStatusReason); r != "" && !strings.Contains(r, "Resource creation cancelled") {
				return aws.ToString(e.LogicalResourceId) + ": " + r
			}
		}
	}
	return "see the CloudFormation console"
}

// EmptyBucket deletes every object (and version) in a bucket.
func EmptyBucket(ctx context.Context, cfg aws.Config, bucket string, prefix string) (int, error) {
	c := s3.NewFromConfig(cfg)
	n := 0
	p := s3.NewListObjectVersionsPaginator(c, &s3.ListObjectVersionsInput{Bucket: &bucket, Prefix: aws.String(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			if strings.Contains(err.Error(), "NoSuchBucket") {
				return n, nil
			}
			return n, err
		}
		var objs []s3types.ObjectIdentifier
		for _, v := range page.Versions {
			objs = append(objs, s3types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
		}
		for _, m := range page.DeleteMarkers {
			objs = append(objs, s3types.ObjectIdentifier{Key: m.Key, VersionId: m.VersionId})
		}
		for i := 0; i < len(objs); i += 1000 {
			j := min(i+1000, len(objs))
			if _, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &bucket, Delete: &s3types.Delete{Objects: objs[i:j], Quiet: aws.Bool(true)}}); err != nil {
				return n, err
			}
			n += j - i
		}
	}
	return n, nil
}

// DeleteBucket empties and removes a bucket.
func DeleteBucket(ctx context.Context, cfg aws.Config, bucket string) error {
	if _, err := EmptyBucket(ctx, cfg, bucket, ""); err != nil {
		return err
	}
	_, err := s3.NewFromConfig(cfg).DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket})
	if err != nil && strings.Contains(err.Error(), "NoSuchBucket") {
		return nil
	}
	return err
}

// ListStacks returns the names of all truner stacks in the region.
func ListStacks(ctx context.Context, cfg aws.Config) ([]string, error) {
	var out []string
	p := cloudformation.NewDescribeStacksPaginator(cloudformation.NewFromConfig(cfg), &cloudformation.DescribeStacksInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range page.Stacks {
			for _, t := range s.Tags {
				if aws.ToString(t.Key) == "truner:managed" && s.StackStatus != cftypes.StackStatusDeleteComplete {
					out = append(out, aws.ToString(s.StackName))
				}
			}
		}
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
