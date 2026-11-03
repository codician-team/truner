package controlplane

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	mvm "github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	mvmtypes "github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/codician-team/truner/internal/agentcfg"
	"github.com/codician-team/truner/internal/config"
)

// Micro jobs run on Lambda MicroVMs. truner keeps one MicroVM image per size
// (1, 2 and 4 vCPU), built from a Dockerfile, the agent and the runner the
// first time a job asks for that size. Lambda snapshots each image once the
// agent has the runner ready, and every job gets a MicroVM resumed from that
// snapshot. Until an image is ready, and whenever a MicroVM cannot be
// started, the job runs on an EC2 machine of the same size instead.

const collMicro = "micro-images"

// Lambda MicroVM list prices in us-east-1 (Graviton): baseline vCPU and memory
// per hour while running, and snapshot data read when a MicroVM starts.
const (
	microVCPUHour   = 0.0000276944 * 3600
	microGiBHour    = 0.0000036667 * 3600
	microReadPerGiB = 0.00155
)

// microDockerfile builds the image the snapshot is taken from: Ubuntu 24.04
// with the tools short jobs commonly use. The agent installs the runner when
// it starts, before Lambda calls /ready.
const microDockerfile = `FROM public.ecr.aws/ubuntu/ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl wget git git-lfs gnupg jq less lsb-release \
    make file openssh-client python3 python3-pip python3-venv rsync sudo tar gzip unzip zip xz-utils zstd libicu74 \
 && rm -rf /var/lib/apt/lists/*
COPY truner-agent /usr/local/bin/truner-agent
COPY microvm.json /etc/truner/microvm.json
RUN chmod 0755 /usr/local/bin/truner-agent
ENTRYPOINT ["/usr/local/bin/truner-agent", "microvm", "--config", "/etc/truner/microvm.json"]
`

func microPK(vcpu int) string { return fmt.Sprintf("micro#%d", vcpu) }

func (cp *ControlPlane) microImageName(vcpu int) string {
	return fmt.Sprintf("%s-micro-%d", cp.Env.Stack, vcpu)
}

// MicroEnabled reports whether this installation has the MicroVM roles.
func (cp *ControlPlane) MicroEnabled() bool {
	return cp.Env.MicroBuildRoleArn != "" && cp.Env.MicroExecRoleArn != ""
}

// isMicroVM tells MicroVM ids from EC2 instance ids, which start with "i-".
func isMicroVM(id string) bool { return id != "" && !strings.HasPrefix(id, "i-") }

// MicroHourlyPrice is the baseline price of a MicroVM per hour.
func MicroHourlyPrice(vcpu int) float64 {
	return float64(vcpu)*microVCPUHour + float64(2*vcpu)*microGiBHour
}

// MicroStartPrice estimates the snapshot data a MicroVM reads when it starts.
func MicroStartPrice(vcpu int) float64 { return float64(2*vcpu) * microReadPerGiB }

// connector is a Lambda-managed network connector.
func (cp *ControlPlane) connector(name string) string {
	return fmt.Sprintf("arn:aws:lambda:%s:aws:network-connector:aws-network-connector:%s", cp.Env.Region, name)
}

// microImage is a MicroVM image jobs can run on.
type microImage struct {
	VCPU          int
	Arn           string
	ImageVersion  string
	RunnerVersion string
}

// MicroImage returns the ready MicroVM image of a size. It starts a build
// when the image is missing or was built for another truner or runner
// version; reason says why no image is returned.
func (cp *ControlPlane) MicroImage(ctx context.Context, vcpu int, rel *runnerRelease) (img *microImage, reason string) {
	if !cp.MicroEnabled() {
		return nil, "this installation has no MicroVM roles yet; run `truner upgrade`"
	}
	it, err := cp.Store.GetItem(ctx, microPK(vcpu))
	if err != nil {
		return nil, "could not read the MicroVM image state: " + err.Error()
	}
	if it == nil {
		it = Item{}
	}
	usable := it.Str("imageVersion") != "" && it.Str("agent") == cp.Env.Version
	stale := !usable || rel != nil && it.Str("runner") != rel.Version
	age := time.Since(time.Unix(it.Int("ts"), 0))
	status := it.Str("status")
	busy := status == "building" && age < 2*time.Hour
	// After a failure, wait before building again, unless truner was upgraded
	// since: an upgrade may well have fixed the cause.
	backoff := status == "failed" && age < time.Hour && it.Str("failedAgent") == cp.Env.Version
	if stale && !busy && !backoff && rel != nil {
		if err := cp.startMicroBuild(ctx, vcpu, rel, it); err != nil {
			cp.Log.Warn("MicroVM image build did not start", "vcpu", vcpu, "err", err)
			status, it["error"] = "failed", err.Error()
		} else {
			status = "building"
		}
	}
	if usable {
		return &microImage{VCPU: vcpu, Arn: it.Str("arn"), ImageVersion: it.Str("imageVersion"), RunnerVersion: it.Str("runner")}, ""
	}
	if status == "failed" {
		return nil, fmt.Sprintf("the %d vCPU MicroVM image could not be built (%s); truner tries again within an hour", vcpu, it.Str("error"))
	}
	return nil, fmt.Sprintf("the %d vCPU MicroVM image for truner %s is being built, which takes a few minutes", vcpu, cp.Env.Version)
}

// startMicroBuild creates or updates the MicroVM image of a size.
func (cp *ControlPlane) startMicroBuild(ctx context.Context, vcpu int, rel *runnerRelease, cur Item) error {
	// One build per size at a time, even when several workers get here.
	lock := fmt.Sprintf("%s#lock#%s#%s#%d", microPK(vcpu), cp.Env.Version, rel.Version, time.Now().Unix()/600)
	if ok, err := cp.Store.PutItem(ctx, lock, Item{}, time.Hour, true); err != nil || !ok {
		return err
	}
	key, err := cp.microArtifact(ctx, rel)
	if err != nil {
		return cp.microFailed(ctx, vcpu, err)
	}
	base, err := cp.microBaseImage(ctx)
	if err != nil {
		return cp.microFailed(ctx, vcpu, err)
	}
	name := cp.microImageName(vcpu)
	artifact := &mvmtypes.CodeArtifactMemberUri{Value: "s3://" + cp.Env.Bucket + "/" + key}
	hooks := &mvmtypes.Hooks{
		Port: aws.Int32(agentcfg.MicroHookPort),
		MicrovmImageHooks: &mvmtypes.MicrovmImageHooks{
			Ready: mvmtypes.HookStateEnabled, ReadyTimeoutInSeconds: aws.Int32(900),
			Validate: mvmtypes.HookStateEnabled, ValidateTimeoutInSeconds: aws.Int32(300),
		},
		MicrovmHooks: &mvmtypes.MicrovmHooks{
			Run: mvmtypes.HookStateEnabled, RunTimeoutInSeconds: aws.Int32(60),
			Terminate: mvmtypes.HookStateEnabled, TerminateTimeoutInSeconds: aws.Int32(30),
		},
	}
	resources := []mvmtypes.Resources{{MinimumMemoryInMiB: aws.Int32(int32(vcpu * 2048))}}
	cpus := []mvmtypes.CpuConfiguration{{Architecture: mvmtypes.ArchitectureArm64}}
	// Jobs have root on EC2 runners too (sudo, mounts, apt); the extra
	// capabilities stay inside the MicroVM, which runs a single job.
	caps := []mvmtypes.Capability{mvmtypes.CapabilityAll}
	desc := fmt.Sprintf("truner %s micro runner, %d vCPU, actions/runner %s", cp.Env.Version, vcpu, rel.Version)
	token := clientToken(cp.Env.Stack, fmt.Sprint(vcpu), cp.Env.Version, rel.Version, fmt.Sprint(time.Now().Unix()/3600))
	var logging mvmtypes.Logging
	if cp.Env.MicroLogGroup != "" {
		logging = &mvmtypes.LoggingMemberCloudWatch{Value: mvmtypes.CloudWatchLogging{LogGroup: aws.String(cp.Env.MicroLogGroup)}}
	}

	update := func(id string) (string, string, error) {
		out, err := cp.Micro.UpdateMicrovmImage(ctx, &mvm.UpdateMicrovmImageInput{
			ImageIdentifier: aws.String(id), BaseImageArn: aws.String(base), BuildRoleArn: aws.String(cp.Env.MicroBuildRoleArn),
			CodeArtifact: artifact, Hooks: hooks, Resources: resources, CpuConfigurations: cpus, Description: aws.String(desc),
			AdditionalOsCapabilities: caps, Logging: logging, ClientToken: aws.String(token),
		})
		if err != nil {
			return "", "", err
		}
		return aws.ToString(out.ImageArn), aws.ToString(out.ImageVersion), nil
	}
	var arn, version string
	if id := cur.Str("arn"); id != "" {
		arn, version, err = update(id)
	} else {
		var out *mvm.CreateMicrovmImageOutput
		out, err = cp.Micro.CreateMicrovmImage(ctx, &mvm.CreateMicrovmImageInput{
			Name: aws.String(name), BaseImageArn: aws.String(base), BuildRoleArn: aws.String(cp.Env.MicroBuildRoleArn),
			CodeArtifact: artifact, Hooks: hooks, Resources: resources, CpuConfigurations: cpus, Description: aws.String(desc),
			AdditionalOsCapabilities: caps, Logging: logging, ClientToken: aws.String(token),
			Tags: map[string]string{TagManaged: "true", TagStack: cp.Env.Stack},
		})
		var conflict *mvmtypes.ConflictException
		if errors.As(err, &conflict) {
			// The image exists but its record was lost.
			arn, version, err = update(name)
		} else if err == nil {
			arn, version = aws.ToString(out.ImageArn), aws.ToString(out.ImageVersion)
		}
	}
	if err != nil {
		return cp.microFailed(ctx, vcpu, err)
	}
	cp.Log.Info("MicroVM image build started", "image", name, "version", version, "runner", rel.Version)
	return cp.Store.UpdateItem(ctx, microPK(vcpu), Item{
		"coll": collMicro, "ts": time.Now().Unix(), "vcpu": vcpu, "arn": arn, "status": "building", "error": "",
		"buildVersion": version, "buildAgent": cp.Env.Version, "buildRunner": rel.Version,
	})
}

func (cp *ControlPlane) microFailed(ctx context.Context, vcpu int, err error) error {
	cp.Store.UpdateItem(ctx, microPK(vcpu), Item{"coll": collMicro, "ts": time.Now().Unix(), "vcpu": vcpu, "status": "failed",
		"failedAgent": cp.Env.Version, "error": truncate(err.Error(), 500)})
	return err
}

func clientToken(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:16])
}

// microBaseImage picks the Lambda-managed Amazon Linux base of MicroVMs.
func (cp *ControlPlane) microBaseImage(ctx context.Context) (string, error) {
	out, err := cp.Micro.ListManagedMicrovmImages(ctx, &mvm.ListManagedMicrovmImagesInput{})
	if err != nil {
		return "", fmt.Errorf("list MicroVM base images (are Lambda MicroVMs available in %s?): %w", cp.Env.Region, err)
	}
	var first string
	for _, it := range out.Items {
		arn := aws.ToString(it.ImageArn)
		if strings.Contains(arn, "al2023") {
			return arn, nil
		}
		if first == "" {
			first = arn
		}
	}
	if first == "" {
		return "", errors.New("no Lambda-managed MicroVM base image in this region")
	}
	return first, nil
}

// microArtifact uploads the image's build context (Dockerfile, agent and its
// config) for this truner and runner version, once.
func (cp *ControlPlane) microArtifact(ctx context.Context, rel *runnerRelease) (string, error) {
	key := fmt.Sprintf("microvm/%s/runner-%s.zip", cp.Env.Version, rel.Version)
	if _, err := cp.S3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &cp.Env.Bucket, Key: &key}); err == nil {
		return key, nil
	}
	bucket := cp.Env.ArtifactsBucket
	if bucket == "" {
		bucket = cp.Env.Bucket
	}
	obj, err := cp.S3.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: aws.String(agentKey(cp.Env.Version, config.PlatformLinux, config.ArchARM64))})
	if err != nil {
		return "", fmt.Errorf("read the arm64 agent: %w", err)
	}
	agentBin, err := io.ReadAll(obj.Body)
	obj.Body.Close()
	if err != nil {
		return "", err
	}
	cfg, _ := json.MarshalIndent(cp.microImageConfig(ctx, rel), "", "  ")
	data, err := microZip(agentBin, cfg)
	if err != nil {
		return "", err
	}
	_, err = cp.S3.PutObject(ctx, &s3.PutObjectInput{Bucket: &cp.Env.Bucket, Key: &key, Body: bytes.NewReader(data), ContentType: aws.String("application/zip")})
	return key, err
}

// microImageConfig is what the agent in a MicroVM image starts with.
func (cp *ControlPlane) microImageConfig(ctx context.Context, rel *runnerRelease) *agentcfg.Config {
	return &agentcfg.Config{
		Version: cp.Env.Version, Mode: agentcfg.ModeMicro, Platform: config.PlatformLinux, Arch: config.ArchARM64,
		Stack: cp.Env.Stack, Region: cp.Env.Region, Bucket: cp.Env.Bucket,
		RunnerVersion: rel.Version, RunnerURLs: cp.runnerURLs(ctx, rel, config.PlatformLinux, config.ArchARM64),
		Profile: "none", ImageName: config.MicroImage, ImageOS: "ubuntu24", User: microUser,
	}
}

// microUser runs the runner in MicroVMs. Their container sets no_new_privs,
// so sudo cannot raise a user's privileges there: jobs run as root instead,
// where sudo works.
const microUser = "root"

func microZip(agentBin, cfg []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range []struct {
		name string
		mode uint32
		data []byte
	}{{"Dockerfile", 0o644, []byte(microDockerfile)}, {"truner-agent", 0o755, agentBin}, {"microvm.json", 0o644, cfg}} {
		h := &zip.FileHeader{Name: f.name, Method: zip.Deflate}
		h.SetMode(fs.FileMode(f.mode))
		w, err := zw.CreateHeader(h)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ProcessMicroImages follows MicroVM image builds and removes image versions
// no MicroVM can still be running.
func (cp *ControlPlane) ProcessMicroImages(ctx context.Context) error {
	items, err := cp.Store.ItemsInColl(ctx, collMicro)
	if err != nil {
		return err
	}
	var errs []error
	for _, it := range items {
		if err := cp.processMicroImage(ctx, it); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (cp *ControlPlane) processMicroImage(ctx context.Context, it Item) error {
	pk, arn := it.Str("pk"), it.Str("arn")
	cp.retireMicroVersions(ctx, it)
	if it.Str("status") != "building" || arn == "" {
		return nil
	}
	v, err := cp.Micro.GetMicrovmImageVersion(ctx, &mvm.GetMicrovmImageVersionInput{ImageIdentifier: aws.String(arn), ImageVersion: aws.String(it.Str("buildVersion"))})
	if err != nil {
		return err
	}
	switch v.State {
	case mvmtypes.MicrovmImageVersionStateSuccessful:
		set := Item{"status": "ready", "ts": time.Now().Unix(), "imageVersion": it.Str("buildVersion"),
			"agent": it.Str("buildAgent"), "runner": it.Str("buildRunner"), "error": ""}
		if old := it.Str("imageVersion"); old != "" && old != it.Str("buildVersion") {
			set["retired"] = appendRetired(it.Str("retired"), old, time.Now())
		}
		cp.Log.Info("MicroVM image ready", "image", arn, "version", it.Str("buildVersion"))
		return cp.Store.UpdateItem(ctx, pk, set)
	case mvmtypes.MicrovmImageVersionStateFailed:
		reason := aws.ToString(v.StateReason)
		if reason == "" {
			reason = "build failed; see the CloudWatch log group " + cp.Env.MicroLogGroup
		}
		cp.Log.Warn("MicroVM image build failed", "image", arn, "version", it.Str("buildVersion"), "reason", reason)
		return cp.Store.UpdateItem(ctx, pk, Item{"status": "failed", "ts": time.Now().Unix(), "failedAgent": it.Str("buildAgent"), "error": truncate(reason, 500)})
	}
	if time.Since(time.Unix(it.Int("ts"), 0)) > 2*time.Hour {
		return cp.Store.UpdateItem(ctx, pk, Item{"status": "failed", "ts": time.Now().Unix(), "failedAgent": it.Str("buildAgent"), "error": "the build did not finish within 2 hours"})
	}
	return nil
}

// A replaced image version is deleted once no MicroVM started from it can be
// alive: MicroVMs live at most 8 hours.
const microRetireAfter = 9 * time.Hour

func appendRetired(list, version string, at time.Time) string {
	e := fmt.Sprintf("%s@%d", version, at.Unix())
	if list == "" {
		return e
	}
	return list + "," + e
}

func (cp *ControlPlane) retireMicroVersions(ctx context.Context, it Item) {
	list := it.Str("retired")
	if list == "" {
		return
	}
	var keep []string
	for _, e := range strings.Split(list, ",") {
		version, at, _ := strings.Cut(e, "@")
		var ts int64
		fmt.Sscan(at, &ts)
		if time.Since(time.Unix(ts, 0)) < microRetireAfter {
			keep = append(keep, e)
			continue
		}
		_, err := cp.Micro.DeleteMicrovmImageVersion(ctx, &mvm.DeleteMicrovmImageVersionInput{ImageIdentifier: aws.String(it.Str("arn")), ImageVersion: aws.String(version)})
		var nf *mvmtypes.ResourceNotFoundException
		if err != nil && !errors.As(err, &nf) {
			cp.Log.Warn("could not delete an old MicroVM image version", "image", it.Str("arn"), "version", version, "err", err)
			keep = append(keep, e)
		}
	}
	if len(keep) != len(strings.Split(list, ",")) {
		cp.Store.UpdateItem(ctx, it.Str("pk"), Item{"retired": strings.Join(keep, ",")})
	}
}

// launchMicro starts a MicroVM for a job. A nil result with a reason means
// the job should run on EC2 instead.
func (cp *ControlPlane) launchMicro(ctx context.Context, job *Job, spec *config.JobSpec, img *microImage, ac *agentcfg.Config) (*LaunchResult, string) {
	payload, err := cp.microPayload(ctx, ac)
	if err != nil {
		return nil, "its runner config could not be stored: " + err.Error()
	}
	maxSec := min((spec.Timeout+10)*60, 28800)
	in := &mvm.RunMicrovmInput{
		ImageIdentifier: aws.String(img.Arn), ImageVersion: aws.String(img.ImageVersion),
		ExecutionRoleArn: aws.String(cp.Env.MicroExecRoleArn),
		RunHookPayload:   aws.String(payload),
		// Runners only connect out, so the endpoint sees no traffic: an idle
		// policy must never suspend them. The maximum duration ends them.
		IdlePolicy:               &mvmtypes.IdlePolicy{AutoResumeEnabled: aws.Bool(false), MaxIdleDurationSeconds: aws.Int32(28800), SuspendedDurationSeconds: aws.Int32(28800)},
		MaximumDurationInSeconds: aws.Int32(int32(maxSec)),
		IngressNetworkConnectors: []string{cp.connector("ALL_INGRESS")},
		EgressNetworkConnectors:  []string{cp.connector("INTERNET_EGRESS")},
		ClientToken:              aws.String(ac.RunnerName),
	}
	if cp.Env.MicroLogGroup != "" {
		in.Logging = &mvmtypes.LoggingMemberCloudWatch{Value: mvmtypes.CloudWatchLogging{LogGroup: aws.String(cp.Env.MicroLogGroup)}}
	}
	start := time.Now()
	out, err := cp.Micro.RunMicrovm(ctx, in)
	if err != nil {
		var quota *mvmtypes.ServiceQuotaExceededException
		var nf *mvmtypes.ResourceNotFoundException
		switch {
		case errors.As(err, &quota):
			cp.emit("MicroQuotaErrors", 1, UnitCount)
			return nil, "the account's MicroVM quota is used up (raise it in Service Quotas under Lambda MicroVMs)"
		case errors.As(err, &nf):
			// The image or version is gone: build it again.
			cp.Store.UpdateItem(ctx, microPK(img.VCPU), Item{"arn": "", "imageVersion": "", "status": "failed", "ts": 0, "error": "image not found"})
			return nil, "its MicroVM image no longer exists and is being rebuilt"
		}
		cp.Log.Warn("RunMicrovm failed", "job", job.JobID, "err", err)
		return nil, "a MicroVM could not be started: " + truncate(err.Error(), 300)
	}
	return &LaunchResult{
		InstanceID: aws.ToString(out.MicrovmId), InstanceType: fmt.Sprintf("microvm-%dvcpu", img.VCPU),
		Lifecycle: "microvm", Took: time.Since(start),
	}, ""
}

// microPayload stores a job's agent config and returns the run hook payload
// that points to it: Lambda limits the payload to 4096 characters, less than
// a runner registration takes. The URL reads this one object only, so a job
// cannot read the registration of another.
func (cp *ControlPlane) microPayload(ctx context.Context, ac *agentcfg.Config) (string, error) {
	enc, err := agentcfg.Encode(ac)
	if err != nil {
		return "", err
	}
	key := "microvm/run/" + ac.RunnerName + ".cfg"
	if _, err := cp.S3.PutObject(ctx, &s3.PutObjectInput{Bucket: &cp.Env.Bucket, Key: &key, Body: strings.NewReader(enc)}); err != nil {
		return "", err
	}
	url, err := cp.presign(ctx, cp.Env.Bucket, key, 15*time.Minute)
	if err != nil {
		return "", err
	}
	if len(url) > microPayloadMax {
		return "", fmt.Errorf("the config URL is too long for a MicroVM (%d characters)", len(url))
	}
	return url, nil
}

// microPayloadMax is the longest run hook payload RunMicrovm accepts.
const microPayloadMax = 4096

// microAgentConfig adapts a job's agent config to a MicroVM.
func microAgentConfig(ac *agentcfg.Config, img *microImage) {
	ac.Mode = agentcfg.ModeMicro
	ac.MicroVCPU = img.VCPU
	ac.User = microUser
	ac.UseNVMe = false
	ac.ECRRegistry, ac.ECRRepo = "", "" // no Docker in a MicroVM
	ac.SSHUsers = nil
	ac.KeepMinutes = 0
	ac.Pool = ""
	if img.RunnerVersion != "" && img.RunnerVersion != ac.RunnerVersion {
		// The image's runner starts right away; a newer one is on its way in
		// the next image build.
		ac.RunnerVersion = img.RunnerVersion
		ac.RunnerURLs = []string{fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/%s", img.RunnerVersion, runnerFile(img.RunnerVersion, config.PlatformLinux, config.ArchARM64))}
	}
}

// LiveMicrovms lists this stack's MicroVMs that are not terminating. It
// asks Lambda only once a MicroVM image exists, so regions without MicroVMs
// see no failing calls.
func (cp *ControlPlane) LiveMicrovms(ctx context.Context) (map[string]mvmtypes.MicrovmItem, error) {
	out := map[string]mvmtypes.MicrovmItem{}
	if !cp.MicroEnabled() {
		return out, nil
	}
	if items, err := cp.Store.ItemsInColl(ctx, collMicro); err != nil || len(items) == 0 {
		return out, err
	}
	mine := ":microvm-image:" + cp.Env.Stack + "-micro-"
	p := mvm.NewListMicrovmsPaginator(cp.Micro, &mvm.ListMicrovmsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			if !strings.Contains(aws.ToString(it.ImageArn), mine) {
				continue
			}
			if it.State == mvmtypes.MicrovmStateTerminating || it.State == mvmtypes.MicrovmStateTerminated {
				continue
			}
			out[aws.ToString(it.MicrovmId)] = it
		}
	}
	return out, nil
}

// terminateMicrovms ends MicroVMs, ignoring ones that are already gone.
func (cp *ControlPlane) terminateMicrovms(ctx context.Context, ids ...string) error {
	var errs []error
	for _, id := range ids {
		_, err := cp.Micro.TerminateMicrovm(ctx, &mvm.TerminateMicrovmInput{MicrovmIdentifier: aws.String(id)})
		var nf *mvmtypes.ResourceNotFoundException
		if err != nil && !errors.As(err, &nf) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reconcileMicrovms ends MicroVMs whose job is over or that no job knows.
// GitHub's completed webhook normally ends them within seconds.
func (cp *ControlPlane) reconcileMicrovms(ctx context.Context, live map[string]mvmtypes.MicrovmItem, jobs []Job) {
	byMachine := map[string]*Job{}
	for i := range jobs {
		if isMicroVM(jobs[i].InstanceID) {
			byMachine[jobs[i].InstanceID] = &jobs[i]
		}
	}
	now := time.Now()
	for id, m := range live {
		age := now.Sub(aws.ToTime(m.StartedAt))
		j := byMachine[id]
		var why string
		switch {
		case j == nil && age > 15*time.Minute:
			why = "no job launched it"
		case j != nil && j.MachineDone > 0 && now.Sub(time.Unix(j.MachineDone, 0)) > 2*time.Minute:
			why = "its job is over"
		case j != nil && (j.Status == StatusCancelled || j.Status == StatusFailed) && age > 5*time.Minute &&
			j.PickedBy == 0: // a picked one runs another job with the same labels
			why = "its job was " + j.Status
		}
		if why != "" {
			cp.Log.Info("terminating MicroVM", "microvm", id, "reason", why)
			if err := cp.terminateMicrovms(ctx, id); err != nil {
				cp.Log.Warn("terminate MicroVM failed", "microvm", id, "err", err)
			}
		}
	}
}

// machineCost estimates what the machine launched for a job cost until t.
func (j *Job) machineCost(t time.Time) float64 {
	d := t.Sub(time.Unix(j.LaunchedAt, 0))
	if !isMicroVM(j.InstanceID) {
		return jobCost(j.Price, j.DiskGiB, d)
	}
	// Billed per second, plus reading the snapshot at the start.
	return j.Price*max(d, 0).Hours() + MicroStartPrice(max(j.CPU, 1))
}

// RemoveMicroVMs terminates this stack's MicroVMs and deletes its MicroVM
// images, for uninstall. It returns how many images it deleted.
func (cp *ControlPlane) RemoveMicroVMs(ctx context.Context) (int, error) {
	prefix := cp.Env.Stack + "-micro-"
	var arns []string
	p := mvm.NewListMicrovmImagesPaginator(cp.Micro, &mvm.ListMicrovmImagesInput{NameFilter: aws.String(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return 0, err
		}
		for _, it := range page.Items {
			if strings.HasPrefix(aws.ToString(it.Name), prefix) {
				arns = append(arns, aws.ToString(it.ImageArn))
			}
		}
	}
	deleted := 0
	var errs []error
	for _, arn := range arns {
		ms := mvm.NewListMicrovmsPaginator(cp.Micro, &mvm.ListMicrovmsInput{ImageIdentifier: aws.String(arn)})
		for ms.HasMorePages() {
			page, err := ms.NextPage(ctx)
			if err != nil {
				errs = append(errs, err)
				break
			}
			for _, m := range page.Items {
				if m.State != mvmtypes.MicrovmStateTerminated {
					errs = append(errs, cp.terminateMicrovms(ctx, aws.ToString(m.MicrovmId)))
				}
			}
		}
		// Deleting waits until the image's MicroVMs are gone.
		for attempt := 0; ; attempt++ {
			_, err := cp.Micro.DeleteMicrovmImage(ctx, &mvm.DeleteMicrovmImageInput{ImageIdentifier: aws.String(arn)})
			var conflict *mvmtypes.ConflictException
			if errors.As(err, &conflict) && attempt < 12 {
				time.Sleep(5 * time.Second)
				continue
			}
			if err == nil {
				deleted++
			}
			errs = append(errs, err)
			break
		}
	}
	return deleted, errors.Join(errs...)
}
