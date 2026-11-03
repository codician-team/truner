package controlplane

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codician-team/truner/internal/agentcfg"
	"github.com/codician-team/truner/internal/config"
)

func TestIntegrationMicro(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	labels := []string{"truner/micro/run=900"}
	warnings := func() string { return strings.Join(h.lastUserData().Job.Warnings, " ") }

	// First micro job: no image yet. The build starts and the job runs on a
	// Graviton EC2 machine of the same size.
	h.webhook("queued", 7001, labels, "", "")
	h.drainQueue()
	if j := h.job(7001); j.Status != StatusLaunched || isMicroVM(j.InstanceID) {
		t.Fatalf("first micro job must fall back to EC2: %+v", j)
	}
	if ac := h.lastUserData(); ac.Mode != agentcfg.ModeRun || ac.Arch != config.ArchARM64 || !strings.Contains(warnings(), "runs on an EC2 machine") {
		t.Fatalf("fallback agent config: %+v", ac)
	}
	if len(h.aws.microBuilds) != 1 {
		t.Fatalf("expected one image build, got %d", len(h.aws.microBuilds))
	}
	b := h.aws.microBuilds[0]
	hooks := b["hooks"].(map[string]any)
	if b["name"] != "test-micro-2" || b["buildRoleArn"] != "arn:aws:iam::1:role/microvm-build" || hooks["port"] != float64(9000) ||
		b["resources"].([]any)[0].(map[string]any)["minimumMemoryInMiB"] != float64(4096) ||
		b["cpuConfigurations"].([]any)[0].(map[string]any)["architecture"] != "ARM_64" || b["additionalOsCapabilities"].([]any)[0] != "ALL" ||
		b["codeArtifact"].(map[string]any)["uri"] != "s3://data/microvm/v-test/runner-2.338.2.zip" {
		t.Fatalf("unexpected image build request: %v", b)
	}
	art := h.aws.objects["data/microvm/v-test/runner-2.338.2.zip"]
	zr, err := zip.NewReader(bytes.NewReader(art), int64(len(art)))
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(data)
	}
	if !strings.Contains(files["Dockerfile"], `/usr/local/bin/truner-agent", "microvm"`) || files["truner-agent"] != "fake arm64 agent" || !strings.Contains(files["microvm.json"], `"runnerVersion": "2.338.2"`) || !strings.Contains(files["microvm.json"], `"user": "root"`) {
		t.Fatalf("unexpected build context: %v", files)
	}

	// A second job while the image builds: no second build.
	h.webhook("queued", 7002, labels, "", "")
	h.drainQueue()
	if len(h.aws.microBuilds) != 1 || isMicroVM(h.job(7002).InstanceID) {
		t.Fatalf("builds: %d, job %+v", len(h.aws.microBuilds), h.job(7002))
	}

	// The build finishes; the janitor marks the image ready.
	h.aws.mu.Lock()
	h.aws.microImages["test-micro-2"].versions["1"] = "SUCCESSFUL"
	h.aws.mu.Unlock()
	if err := h.cp.ProcessMicroImages(ctx); err != nil {
		t.Fatal(err)
	}
	if it, _ := h.cp.Store.GetItem(ctx, microPK(2)); it.Str("status") != "ready" || it.Str("imageVersion") != "1" {
		t.Fatalf("image not ready: %v", it)
	}

	// Now micro jobs get a MicroVM.
	h.webhook("queued", 7003, labels, "", "")
	h.drainQueue()
	j := h.job(7003)
	if !isMicroVM(j.InstanceID) || j.InstanceType != "microvm-2vcpu" || j.Lifecycle != "microvm" || j.DiskGiB != 0 || j.Price != MicroHourlyPrice(2) {
		t.Fatalf("unexpected MicroVM launch record: %+v", j)
	}
	run := h.aws.microRuns[len(h.aws.microRuns)-1]
	idle := run["idlePolicy"].(map[string]any)
	if run["imageIdentifier"] != h.aws.microImages["test-micro-2"].arn || run["imageVersion"] != "1" || run["executionRoleArn"] != "arn:aws:iam::1:role/microvm-exec" ||
		idle["autoResumeEnabled"] != false || idle["maxIdleDurationSeconds"] != float64(28800) || run["maximumDurationInSeconds"] != float64(28800) ||
		!strings.HasSuffix(run["egressNetworkConnectors"].([]any)[0].(string), ":INTERNET_EGRESS") ||
		run["logging"].(map[string]any)["cloudWatch"].(map[string]any)["logGroup"] != "/truner/test/microvms" {
		t.Fatalf("unexpected RunMicrovm request: %v", run)
	}
	// The payload points to the config, which is too large for it.
	u, err := url.Parse(run["runHookPayload"].(string))
	if err != nil || !strings.HasPrefix(u.Path, "/data/microvm/run/truner-7003-") || u.Query().Get("X-Amz-Expires") != "900" {
		t.Fatalf("payload must be a presigned URL of the config: %v", run["runHookPayload"])
	}
	mc, err := agentcfg.Decode(string(h.aws.objects[strings.TrimPrefix(u.Path, "/")]))
	if err != nil {
		t.Fatal(err)
	}
	if mc.Mode != agentcfg.ModeMicro || mc.MicroVCPU != 2 || !strings.HasPrefix(mc.JITConfig, "JITCFG-truner-7003-") || mc.RunnerVersion != "2.338.2" ||
		mc.User != "root" || mc.ECRRegistry != "" || mc.UseNVMe || !mc.Cache.Enabled || mc.Arch != config.ArchARM64 || mc.ImageName != config.MicroImage {
		t.Fatalf("unexpected MicroVM agent config: %+v", mc)
	}

	// The job runs and completes: the MicroVM is terminated right away.
	h.webhook("in_progress", 7003, labels, j.RunnerName, "")
	h.webhook("completed", 7003, labels, j.RunnerName, "success")
	if st := h.aws.microvms[j.InstanceID].state; st != "TERMINATED" {
		t.Fatalf("MicroVM must be terminated when its job completes, state %s", st)
	}
	if j := h.job(7003); j.Status != StatusCompleted || j.Cost <= 0 || j.RanOnType != "microvm-2vcpu" {
		t.Fatalf("completed micro job: %+v", j)
	}

	// A MicroVM that dies before taking its job: the retry uses EC2.
	h.webhook("queued", 7004, labels, "", "")
	h.drainQueue()
	j = h.job(7004)
	h.aws.mu.Lock()
	h.aws.microvms[j.InstanceID].state = "TERMINATED"
	// And a MicroVM of this stack that no job knows about.
	h.aws.microvms["mvm-orphan"] = &fakeMicrovm{id: "mvm-orphan", image: h.aws.microImages["test-micro-2"].arn, state: "RUNNING", started: time.Now().Add(-time.Hour)}
	h.aws.mu.Unlock()
	h.cp.Store.UpdateJob(ctx, 7004, map[string]any{"launchedAt": time.Now().Add(-5 * time.Minute).Unix()}, nil)
	live, err := h.cp.LiveMicrovms(ctx)
	if err != nil || len(live) != 1 {
		t.Fatalf("live MicroVMs: %v %v", live, err)
	}
	insts, _ := h.cp.ManagedInstances(ctx)
	if err := h.cp.reconcileJobs(ctx, insts, live); err != nil {
		t.Fatal(err)
	}
	if st := h.aws.microvms["mvm-orphan"].state; st != "TERMINATED" {
		t.Fatalf("orphaned MicroVM must be terminated, state %s", st)
	}
	if j := h.job(7004); j.Status != StatusQueued || !j.NoMicro {
		t.Fatalf("expected a relaunch without MicroVM: %+v", j)
	}
	h.drainQueue()
	if j := h.job(7004); j.Status != StatusLaunched || isMicroVM(j.InstanceID) {
		t.Fatalf("relaunch must use EC2: %+v", j)
	}

	// No MicroVM quota left: EC2 again, and the runner registration is removed.
	h.aws.microFail = "ServiceQuotaExceededException"
	deleted := h.gh.deleted
	h.webhook("queued", 7005, labels, "", "")
	h.drainQueue()
	if j := h.job(7005); j.Status != StatusLaunched || isMicroVM(j.InstanceID) || h.gh.deleted != deleted+1 {
		t.Fatalf("quota error must fall back to EC2: %+v (deleted %d)", j, h.gh.deleted-deleted)
	}
	if w := warnings(); !strings.Contains(w, "quota") {
		t.Fatalf("job log must explain the fallback: %v", w)
	}
	h.aws.microFail = ""

	// A truner upgrade rebuilds the image; jobs use EC2 until it is ready,
	// and the replaced version is deleted once no MicroVM can still use it.
	h.cp.Env.Version = "v-next"
	h.aws.objects["artifacts/agent/v-next/truner-agent-linux-arm64"] = []byte("fake arm64 agent")
	h.webhook("queued", 7006, labels, "", "")
	h.drainQueue()
	if len(h.aws.microBuilds) != 2 || isMicroVM(h.job(7006).InstanceID) {
		t.Fatalf("upgrade must rebuild the image: builds %d, job %+v", len(h.aws.microBuilds), h.job(7006))
	}
	h.aws.mu.Lock()
	h.aws.microImages["test-micro-2"].versions["2"] = "SUCCESSFUL"
	h.aws.mu.Unlock()
	h.cp.ProcessMicroImages(ctx)
	if it, _ := h.cp.Store.GetItem(ctx, microPK(2)); it.Str("imageVersion") != "2" || !strings.HasPrefix(it.Str("retired"), "1@") {
		t.Fatalf("new version must replace the old one: %v", it)
	}
	h.cp.Store.UpdateItem(ctx, microPK(2), Item{"retired": "1@" + strconv.FormatInt(time.Now().Add(-10*time.Hour).Unix(), 10)})
	h.cp.ProcessMicroImages(ctx)
	if len(h.aws.microDeleted) != 1 || h.aws.microDeleted[0] != "1" {
		t.Fatalf("old image version must be deleted: %v", h.aws.microDeleted)
	}

	// A failed build is reported and retried later, not on every job.
	h.cp.Env.Version = "v-broken"
	h.aws.objects["artifacts/agent/v-broken/truner-agent-linux-arm64"] = []byte("fake arm64 agent")
	h.webhook("queued", 7007, labels, "", "")
	h.drainQueue()
	h.aws.mu.Lock()
	h.aws.microImages["test-micro-2"].versions["3"] = "FAILED"
	h.aws.mu.Unlock()
	h.cp.ProcessMicroImages(ctx)
	if it, _ := h.cp.Store.GetItem(ctx, microPK(2)); it.Str("status") != "failed" || !strings.Contains(it.Str("error"), "Dockerfile") {
		t.Fatalf("failed build: %v", it)
	}
	h.webhook("queued", 7008, labels, "", "")
	h.drainQueue()
	if len(h.aws.microBuilds) != 3 {
		t.Fatalf("a failed build must not be retried right away: %d builds", len(h.aws.microBuilds))
	}
	if w := warnings(); !strings.Contains(w, "could not be built") {
		t.Fatalf("job log must explain the failed build: %v", w)
	}
	// An upgrade may fix the cause: the next job builds again right away.
	h.cp.Env.Version = "v-fixed"
	h.aws.objects["artifacts/agent/v-fixed/truner-agent-linux-arm64"] = []byte("fake arm64 agent")
	h.webhook("queued", 7009, labels, "", "")
	h.drainQueue()
	if len(h.aws.microBuilds) != 4 {
		t.Fatalf("an upgrade must retry a failed build: %d builds", len(h.aws.microBuilds))
	}

	// Uninstall removes the MicroVM images.
	if n, err := h.cp.RemoveMicroVMs(ctx); err != nil || n != 1 || len(h.aws.microImages) != 0 {
		t.Fatalf("remove MicroVM images: %d %v", n, err)
	}
}

func TestIntegrationMicroImageGone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	labels := []string{"truner/micro/cpu=1"}
	// A ready image whose MicroVM image was deleted outside truner.
	h.cp.Store.UpdateItem(ctx, microPK(1), Item{"coll": collMicro, "ts": time.Now().Unix(), "vcpu": 1, "status": "ready",
		"arn": "arn:aws:lambda:us-east-1:123456789012:microvm-image:test-micro-1", "imageVersion": "4", "agent": "v-test", "runner": "2.338.2"})
	h.aws.microFail = "ResourceNotFoundException"
	h.webhook("queued", 8001, labels, "", "")
	h.drainQueue()
	if j := h.job(8001); j.Status != StatusLaunched || isMicroVM(j.InstanceID) {
		t.Fatalf("job must fall back to EC2: %+v", j)
	}
	if it, _ := h.cp.Store.GetItem(ctx, microPK(1)); it.Str("arn") != "" || it.Str("imageVersion") != "" {
		t.Fatalf("the lost image must be forgotten: %v", it)
	}
	h.aws.microFail = ""
	h.webhook("queued", 8002, labels, "", "")
	h.drainQueue()
	if len(h.aws.microBuilds) != 1 || h.aws.microBuilds[0]["name"] != "test-micro-1" || h.aws.microImages["test-micro-1"] == nil {
		t.Fatalf("the image must be created again: %v", h.aws.microBuilds)
	}
}
