package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/codician-team/truner/internal/agentcfg"
)

func TestLockIsExclusive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for /run/truner")
	}
	if !osLock() {
		t.Fatal("first lock failed")
	}
	if osLock() {
		t.Fatal("a second agent must not get the lock")
	}
}

func TestOwnedBy(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	os.WriteFile(p, nil, 0o644)
	if !ownedBy(p, strconv.Itoa(os.Getuid())) || ownedBy(p, "54321") || ownedBy(p+"x", strconv.Itoa(os.Getuid())) {
		t.Fatal("ownedBy is wrong")
	}
}

func TestHardDiskStorageIsSkipped(t *testing.T) {
	a := &Agent{}
	a.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	a.meta.InstanceType = "d3en.2xlarge"
	if m, err := a.osSetupStorage(context.Background()); m != "" || err != nil {
		t.Fatalf("hard disk instance storage must not be set up: %q %v", m, err)
	}
}

func TestFinalizeFilesOnlyWalksFreshRunners(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to change owners")
	}
	const name = "truner-test"
	if err := exec.Command("id", name).Run(); err != nil {
		if err := exec.Command("useradd", "-m", name).Run(); err != nil {
			t.Skip("cannot create a test user: ", err)
		}
	}
	u, _ := user.Lookup(name)
	uid, _ := strconv.Atoi(u.Uid)
	a := &Agent{Cfg: &agentcfg.Config{User: name}}
	a.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	os.MkdirAll(stateDir(), 0o775) // created by osEnsureUser on a real machine
	dir := t.TempDir()
	deep := filepath.Join(dir, "externals", "node20", "bin", "node")
	for _, p := range []string{filepath.Join(dir, "bin", "Runner.Listener"), deep, filepath.Join(dir, ".env")} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, nil, 0o644)
	}
	owner := func(p string) int { fi, _ := os.Stat(p); return int(fi.Sys().(*syscall.Stat_t).Uid) }

	// A fresh download (bin owned by root) gets the full recursive pass.
	if err := a.osFinalizeFiles(dir); err != nil {
		t.Fatal(err)
	}
	if owner(deep) != uid {
		t.Fatal("a fresh runner must be chowned recursively")
	}
	// A baked runner (bin already owned) only gets the top-level files.
	os.Chown(deep, 0, 0)
	os.Chown(filepath.Join(dir, ".env"), 0, 0)
	if err := a.osFinalizeFiles(dir); err != nil {
		t.Fatal(err)
	}
	if owner(filepath.Join(dir, ".env")) != uid || owner(deep) != 0 {
		t.Fatalf("baked runner: .env owner %d, deep file owner %d", owner(filepath.Join(dir, ".env")), owner(deep))
	}
}
