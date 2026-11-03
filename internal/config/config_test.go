package config

import (
	"strings"
	"testing"

	"github.com/codician-team/truner/internal/labels"
)

func resolve(t *testing.T, r Resolver, raw ...string) *JobSpec {
	t.Helper()
	l, ok := labels.Parse("truner", raw)
	if !ok {
		t.Fatalf("labels %v not matched", raw)
	}
	js, err := r.Resolve(l)
	if err != nil {
		t.Fatalf("resolve %v: %v", raw, err)
	}
	return js
}

func TestDefaults(t *testing.T) {
	js := resolve(t, Resolver{}, "truner")
	if js.CPU.Min != 2 || js.CPU.Max != 2 || js.RAM.Min != 8 || js.Arch != ArchX64 || js.Image != "ubuntu24" || js.Spot != SpotAuto || js.Disk != 40 || js.Cache != CacheS3 {
		t.Fatalf("unexpected defaults: %+v", js)
	}
}

func TestSlashSyntaxAndArray(t *testing.T) {
	a := resolve(t, Resolver{}, "truner/cpu=8/ram=32/arch=arm64/spot=false/run=123")
	b := resolve(t, Resolver{}, "truner", "cpu=8", "ram=32", "arch=arm64", "spot=false", "run=123")
	for _, js := range []*JobSpec{a, b} {
		if js.CPU.Min != 8 || js.RAM.Min != 32 || js.RAM.Max != 0 || js.Arch != ArchARM64 || js.Spot != SpotOff {
			t.Fatalf("unexpected: %+v", js)
		}
	}
	c := resolve(t, Resolver{}, "truner=999/cpu=4")
	if c.CPU.Min != 4 || c.RAM.Min != 16 {
		t.Fatalf("unexpected: %+v", c)
	}
}

func TestRunnerProfileAndOverrides(t *testing.T) {
	js := resolve(t, Resolver{}, "truner/runner=16cpu-linux-arm64/disk=200")
	if js.CPU.Min != 16 || js.RAM.Min != 64 || js.Arch != ArchARM64 || js.Disk != 200 {
		t.Fatalf("unexpected: %+v", js)
	}
}

func TestFamilyDropsDerivedRAM(t *testing.T) {
	js := resolve(t, Resolver{}, "truner/cpu=4/family=c7a+c7i")
	if js.RAM.Min != 0 || len(js.Families) != 2 || js.Families[0] != "c7a*" {
		t.Fatalf("unexpected: %+v", js)
	}
}

func TestImageArchSuffixAndWindows(t *testing.T) {
	js := resolve(t, Resolver{}, "truner/image=ubuntu24-full-arm64")
	if js.Image != "ubuntu24-full" || js.Arch != ArchARM64 || js.ImageSpec.Profile != "full" {
		t.Fatalf("unexpected: %+v", js)
	}
	w := resolve(t, Resolver{}, "truner/runner=4cpu-windows-x64")
	if w.Platform() != PlatformWindows || w.Disk < 50 {
		t.Fatalf("unexpected: %+v", w)
	}
	l, _ := labels.Parse("truner", []string{"truner/image=windows-latest/arch=arm64"})
	if _, err := (Resolver{}).Resolve(l); err == nil {
		t.Fatal("expected windows arm64 to fail")
	}
}

func TestRepoAndGlobalLayers(t *testing.T) {
	g, err := ParseGlobal([]byte(`
defaults:
  disk: 60
runners:
  big:
    extends: 16cpu-linux-x64
    image: ubuntu24-full
pools:
  warm:
    runner: 4cpu-linux-x64
    size: 2
access:
  repos: ["acme/*"]
`))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := ParseRepo([]byte(`
runners:
  big:
    extends: big-base
  big-base:
    cpu: 32
    spot: only
`))
	if err != nil {
		t.Fatal(err)
	}
	r := Resolver{Global: g, Repo: repo}
	js := resolve(t, r, "truner/runner=big")
	if js.CPU.Min != 32 || js.Spot != SpotOnly || js.Disk != 60 {
		t.Fatalf("repo profile should win: %+v", js)
	}
	pool := resolve(t, r, "truner/pool=warm/cpu=64")
	if pool.Pool != "warm" || pool.CPU.Min != 4 || pool.Spot != SpotOff || len(pool.Warnings) == 0 {
		t.Fatalf("pool spec unexpected: %+v", pool)
	}
	if !g.Access.RepoAllowed("ACME/web") || g.Access.RepoAllowed("evil/web") {
		t.Fatal("access rules wrong")
	}
}

func TestInvalid(t *testing.T) {
	for _, raw := range []string{"truner/cpu=abc", "truner/runner=nope", "truner/image=nope", "truner/spot=maybe", "truner/disk=1"} {
		l, _ := labels.Parse("truner", []string{raw})
		if _, err := (Resolver{}).Resolve(l); err == nil {
			t.Errorf("%s: expected error", raw)
		}
	}
	if _, err := ParseGlobal([]byte("runnerz: {}")); err == nil || !strings.Contains(err.Error(), "runnerz") {
		t.Errorf("unknown fields must be rejected: %v", err)
	}
	if _, err := ParseRepo([]byte("runners:\n  a:\n    extends: b\n  b:\n    extends: a\n")); err != nil {
		t.Fatal(err)
	}
	repo, _ := ParseRepo([]byte("runners:\n  a:\n    extends: b\n  b:\n    extends: a\n"))
	if _, err := (Resolver{Repo: repo}).LookupRunner("a"); err == nil {
		t.Error("expected extends cycle error")
	}
}

func TestNotMatched(t *testing.T) {
	if _, ok := labels.Parse("truner", []string{"ubuntu-latest"}); ok {
		t.Fatal("ubuntu-latest must not match")
	}
	if _, ok := labels.Parse("truner", []string{"self-hosted", "trunerx"}); ok {
		t.Fatal("prefix must match exactly")
	}
}
