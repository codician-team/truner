package cli

import (
	"testing"
	"time"

	"github.com/codician-team/truner/internal/controlplane"
)

func TestMachineTaken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	jobs := []controlplane.Job{
		// bake3d run 41669209801: typecheck ran on build's machine and build waited.
		{JobID: 3, Name: "build", TS: ago(10 * time.Minute), Status: controlplane.StatusLaunched, LaunchedAt: ago(10 * time.Minute), PickedBy: 1},
		// Swapped machines, both started: nothing waits.
		{JobID: 4, TS: ago(10 * time.Minute), Status: controlplane.StatusRunning, LaunchedAt: ago(10 * time.Minute), PickedBy: 5, StartedAt: ago(9 * time.Minute)},
		// Its machine was taken a moment ago; a second machine may still be booting.
		{JobID: 6, TS: ago(2 * time.Minute), Status: controlplane.StatusLaunched, LaunchedAt: ago(2 * time.Minute), PickedBy: 7},
		// Ran on its own machine, or the machine is still idle.
		{JobID: 8, TS: ago(time.Hour), Status: controlplane.StatusLaunched, LaunchedAt: ago(time.Hour), PickedBy: 8},
		{JobID: 9, TS: ago(time.Hour), Status: controlplane.StatusLaunched, LaunchedAt: ago(time.Hour)},
		// Cancelled while waiting.
		{JobID: 10, TS: ago(time.Hour), Status: controlplane.StatusCancelled, LaunchedAt: ago(time.Hour), PickedBy: 2},
		{JobID: 11, Name: "newer", TS: ago(6 * time.Minute), Status: controlplane.StatusLaunched, LaunchedAt: ago(6 * time.Minute), PickedBy: 12},
	}
	got := machineTaken(jobs, now, takenGrace)
	if len(got) != 2 || got[0].JobID != 11 || got[1].JobID != 3 {
		t.Fatalf("expected jobs 11 and 3, newest first: %+v", got)
	}
}

func TestRelabel(t *testing.T) {
	if got := relabel("truner/cpu=8/run=41", "truner", "truner-codician"); got != "truner-codician/cpu=8/run=41" {
		t.Errorf("got %q", got)
	}
	if got := relabel("truner=41/cpu=2", "truner", "ci"); got != "ci=41/cpu=2" {
		t.Errorf("got %q", got)
	}
}
