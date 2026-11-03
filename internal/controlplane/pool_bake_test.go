package controlplane

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/codician-team/truner/internal/agentcfg"
	"github.com/codician-team/truner/internal/config"
)

func TestIntegrationWarmPool(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.cp.mu.Lock()
	h.cp.global, h.cp.globalAt = mustGlobal(t, "autoBake: false\npools:\n  fast:\n    runner: 4cpu-linux-x64\n    size: 1\n"), time.Now().Add(time.Hour)
	h.cp.mu.Unlock()

	// The janitor provisions the pool machine.
	insts, _ := h.cp.ManagedInstances(ctx)
	if err := h.cp.ReconcilePools(ctx, insts); err != nil {
		t.Fatal(err)
	}
	var pool *fakeInstance
	for _, in := range h.aws.instances {
		if in.tags[TagPool] == "fast" {
			pool = in
		}
	}
	if pool == nil || pool.tags[TagPoolState] != poolPreparing {
		t.Fatalf("expected a preparing pool machine: %+v", h.aws.instances)
	}
	lt := h.aws.lts[h.aws.fleets[len(h.aws.fleets)-1].Get("LaunchTemplateConfigs.1.LaunchTemplateSpecification.LaunchTemplateId")]
	if got := lt.Get("LaunchTemplateData.InstanceInitiatedShutdownBehavior"); got != "stop" {
		t.Fatalf("pool machines must stop, not terminate: %q", got)
	}
	// The agent finishes preparing: it reports success and stops the machine.
	h.aws.mu.Lock()
	pool.state = "stopped"
	h.aws.objects["data/pools/"+pool.tags[TagPoolID]+".json"] = []byte(`{"ok":true}`)
	h.aws.mu.Unlock()
	insts, _ = h.cp.ManagedInstances(ctx)
	if err := h.cp.ReconcilePools(ctx, insts); err != nil {
		t.Fatal(err)
	}
	if pool.tags[TagPoolState] != poolReady {
		t.Fatalf("pool machine should be ready: %v", pool.tags)
	}
	fleetsBefore := len(h.aws.fleets)

	// A job for the pool claims and starts the machine instead of launching one.
	h.webhook("queued", 7001, []string{"truner/pool=fast/keep=5"}, "", "")
	h.drainQueue()
	j := h.job(7001)
	if j.Status != StatusLaunched || j.InstanceID != pool.id || j.Pool != "fast" || j.RunnerID != 77 || j.KeepMinutes != 5 {
		t.Fatalf("job should run on the pool machine: %+v", j)
	}
	if len(h.aws.fleets) != fleetsBefore {
		t.Fatal("a pool hit must not launch a new machine")
	}
	if pool.state != "running" || pool.tags[TagPoolState] != poolClaimed || pool.tags[TagJobID] != "7001" {
		t.Fatalf("pool machine not claimed: %s %v", pool.state, pool.tags)
	}
	// The janitor replaces the claimed machine to keep the pool full.
	insts, _ = h.cp.ManagedInstances(ctx)
	h.cp.ReconcilePools(ctx, insts)
	if len(h.aws.fleets) != fleetsBefore+1 {
		t.Fatalf("pool should be replenished, fleets %d -> %d", fleetsBefore, len(h.aws.fleets))
	}
}

func TestIntegrationImageBake(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, img, _ := config.BuiltinImage("ubuntu24")
	id, err := h.cp.EnsureBake(ctx, "ubuntu24", img, "x64", false)
	if err != nil || id == "" {
		t.Fatalf("bake: %q %v", id, err)
	}
	if again, _ := h.cp.EnsureBake(ctx, "ubuntu24", img, "x64", false); again != "" {
		t.Fatal("a second bake must not start while one is running")
	}
	it, _ := h.cp.Store.GetItem(ctx, bakePK("ubuntu24", "x64"))
	builder := h.aws.instances[it.Str("builder")]
	if builder == nil || builder.tags[TagRole] != "builder" {
		t.Fatalf("builder not launched: %v", it)
	}
	lt := h.aws.lts[h.aws.fleets[len(h.aws.fleets)-1].Get("LaunchTemplateConfigs.1.LaunchTemplateSpecification.LaunchTemplateId")]
	ud, _ := base64.StdEncoding.DecodeString(lt.Get("LaunchTemplateData.UserData"))
	ac, err := agentcfg.FromUserData(ud)
	if err != nil {
		t.Fatal(err)
	}
	if ac.Mode != agentcfg.ModeBake || ac.Profile != "standard" || ac.MarkerKey != "bakes/"+id+".json" {
		t.Fatalf("bake config: %+v", ac)
	}
	h.aws.mu.Lock()
	builder.state = "stopped"
	h.aws.objects["data/bakes/"+id+".json"] = []byte(`{"ok":true,"runnerVersion":"2.338.2"}`)
	h.aws.mu.Unlock()
	if err := h.cp.ProcessBakes(ctx); err != nil {
		t.Fatal(err)
	}
	it, _ = h.cp.Store.GetItem(ctx, bakePK("ubuntu24", "x64"))
	if it.Str("status") != "imaging" || it.Str("imageId") == "" {
		t.Fatalf("expected imaging: %v", it)
	}
	h.aws.mu.Lock()
	h.aws.images[it.Str("imageId")] = "available"
	h.aws.mu.Unlock()
	if err := h.cp.ProcessBakes(ctx); err != nil {
		t.Fatal(err)
	}
	it, _ = h.cp.Store.GetItem(ctx, bakePK("ubuntu24", "x64"))
	if it.Str("status") != "done" || builder.state != "terminated" {
		t.Fatalf("expected done and builder terminated: %v %s", it, builder.state)
	}
}
