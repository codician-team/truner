package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// MachineLogKey is where a machine's agent uploads its log, by the day it was
// launched (UTC).
func MachineLogKey(runnerName string, launched time.Time) string {
	return fmt.Sprintf("logs/%s/%s.log", launched.UTC().Format("2006-01-02"), runnerName)
}

// ConsoleLogKey is where the control plane keeps a machine's console output,
// next to its log.
func ConsoleLogKey(logKey string) string { return strings.TrimSuffix(logKey, ".log") + ".console.log" }

// saveConsole keeps the console output of an EC2 machine that is terminated
// before its runner took a job. Such a machine often uploads no log: its agent
// uploads when it finishes, and a terminated machine may not get to. The
// console shows how far it booted, and `truner logs <job>` prints it.
// MicroVMs log to CloudWatch instead.
func (cp *ControlPlane) saveConsole(ctx context.Context, j *Job, reason string) {
	if j.InstanceID == "" || isMicroVM(j.InstanceID) || j.RunnerName == "" || j.LaunchedAt == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second) // webhooks answer GitHub within 10 seconds
	defer cancel()
	out, err := cp.EC2.GetConsoleOutput(ctx, &ec2.GetConsoleOutputInput{InstanceId: aws.String(j.InstanceID), Latest: aws.Bool(true)})
	if err != nil {
		// Only Nitro types have the latest output; others have what was buffered.
		out, err = cp.EC2.GetConsoleOutput(ctx, &ec2.GetConsoleOutputInput{InstanceId: aws.String(j.InstanceID)})
	}
	if err != nil {
		cp.Log.Warn("could not read the console output of a machine that took no job", "job", j.JobID, "instance", j.InstanceID, "err", err)
		return
	}
	console, _ := base64.StdEncoding.DecodeString(aws.ToString(out.Output))
	launched := time.Unix(j.LaunchedAt, 0)
	var b bytes.Buffer
	fmt.Fprintf(&b, "===== console output of %s (%s %s), up %s: %s =====\n", j.InstanceID, j.InstanceType, j.Lifecycle,
		time.Since(launched).Round(time.Second), reason)
	if len(console) == 0 {
		b.WriteString("(empty)\n")
	}
	b.Write(console)
	key := ConsoleLogKey(MachineLogKey(j.RunnerName, launched))
	_, err = cp.S3.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(cp.Env.Bucket), Key: aws.String(key), Body: bytes.NewReader(b.Bytes()),
		ContentType: aws.String("text/plain; charset=utf-8")})
	if err != nil {
		cp.Log.Warn("could not save the console output of a machine that took no job", "job", j.JobID, "instance", j.InstanceID, "err", err)
		return
	}
	cp.Log.Info("saved the console output of a machine that took no job", "job", j.JobID, "instance", j.InstanceID, "key", key)
}
