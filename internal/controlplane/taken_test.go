package controlplane

import (
	"slices"
	"testing"
	"time"
)

func TestWithoutMachine(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	l := []string{"truner/cpu=4/run=1"}
	job := func(id int64, status string, labels []string) Job {
		return Job{JobID: id, Owner: "acme", Repo: "app", Labels: labels, Status: status, TS: ago(3 * time.Minute)}
	}
	// waiting: launched, its machine ran another job, not started.
	waiting := func(id, pickedBy int64, labels []string) Job {
		j := job(id, StatusLaunched, labels)
		j.InstanceID, j.LaunchedAt, j.PickedBy = "i-"+string(rune('a'+id)), ago(2*time.Minute), pickedBy
		return j
	}
	// spare: its own job started elsewhere and its machine is unpicked.
	spare := func(id int64, labels []string, instance string, launched time.Duration) Job {
		j := job(id, StatusRunning, labels)
		j.InstanceID, j.LaunchedAt, j.StartedAt = instance, ago(launched), ago(time.Minute)
		return j
	}
	up := func(id string) bool { return id != "i-dead" }
	ids := func(js []*Job) []int64 {
		var out []int64
		for _, j := range js {
			out = append(out, j.JobID)
		}
		return out
	}
	for _, c := range []struct {
		name string
		jobs []Job
		want []int64
	}{
		{"swap: the other machine is coming", []Job{waiting(1, 2, l), spare(2, l, "i-b", time.Minute)}, nil},
		{"late queued event: nothing is coming", []Job{waiting(3, 1, l), job(2, StatusRunning, l)}, []int64{3}},
		{"two waiting, one spare", []Job{waiting(1, 2, l), waiting(3, 4, l), spare(5, l, "i-e", time.Minute)}, []int64{3}},
		{"spare died", []Job{waiting(1, 2, l), spare(2, l, "i-dead", time.Minute)}, []int64{1}},
		{"spare idle too long", []Job{waiting(1, 2, l), spare(2, l, "i-b", 30*time.Minute)}, []int64{1}},
		{"spare MicroVM idle too long", []Job{waiting(1, 2, l), spare(2, l, "mvm-1", 6*time.Minute)}, []int64{1}},
		{"spare in another repository", func() []Job {
			s := spare(2, l, "i-b", time.Minute)
			s.Repo = "other"
			return []Job{waiting(1, 2, l), s}
		}(), []int64{1}},
		{"spare lacks a label", []Job{waiting(1, 2, []string{"self-hosted", "truner/cpu=4"}), spare(2, []string{"truner/cpu=4"}, "i-b", time.Minute)}, []int64{1}},
		{"spare has more labels", []Job{waiting(1, 2, []string{"TRUNER/cpu=4"}), spare(2, []string{"self-hosted", "truner/cpu=4"}, "i-b", time.Minute)}, nil},
		{"ran on its own machine", []Job{waiting(1, 1, l)}, nil},
		{"older than a day", func() []Job {
			w := waiting(1, 2, l)
			w.TS = ago(25 * time.Hour)
			return []Job{w}
		}(), nil},
	} {
		if got := ids(withoutMachine(c.jobs, up, now)); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
