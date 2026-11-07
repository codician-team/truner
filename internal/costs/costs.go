// Package costs summarizes job usage and compares it with GitHub-hosted runners.
package costs

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Usage is one job's billing-relevant facts.
type Usage struct {
	Repo, Workflow, Status  string
	InstanceType, Lifecycle string
	Platform                string
	CPU                     int
	Cost                    float64
	Billed                  bool // Cost comes from the AWS bill, not an estimate
	QueuedAt, StartedAt     int64
	CompletedAt             int64
}

// Summary aggregates usage.
type Summary struct {
	Jobs           int
	LaunchFailures int
	Minutes        float64
	Cost           float64
	Billed         float64 // the part of Cost that comes from the AWS bill
	Estimated      float64 // the part of Cost that is still an estimate
	GitHubCost     float64
	SpotShare      float64
	MedianWait     float64 // seconds from queued to started
	ByRepo         map[string]float64
	ByWorkflow     map[string]float64
	ByType         map[string]float64
}

type githubRate struct {
	cores     int
	perMinute float64
}

// GitHub's list prices for hosted runners, by size (checked 2026-11-07):
// https://docs.github.com/en/billing/reference/actions-runner-pricing
var (
	githubLinuxX64     = []githubRate{{2, 0.006}, {4, 0.012}, {8, 0.022}, {16, 0.042}, {32, 0.082}, {64, 0.162}, {96, 0.252}}
	githubLinuxArm64   = []githubRate{{2, 0.005}, {4, 0.008}, {8, 0.014}, {16, 0.026}, {32, 0.050}, {64, 0.098}}
	githubWindowsX64   = []githubRate{{2, 0.010}, {4, 0.022}, {8, 0.042}, {16, 0.082}, {32, 0.162}, {64, 0.322}, {96, 0.552}}
	githubWindowsArm64 = []githubRate{{2, 0.008}, {4, 0.014}, {8, 0.026}, {16, 0.050}, {32, 0.098}, {64, 0.194}}
)

// GitHubPerMinute returns the list price of the smallest GitHub-hosted runner
// with at least cpu cores. It is only used for comparisons.
func GitHubPerMinute(cpu int, platform, arch string) float64 {
	rates := githubLinuxX64
	switch {
	case platform == "windows" && arch == "arm64":
		rates = githubWindowsArm64
	case platform == "windows":
		rates = githubWindowsX64
	case arch == "arm64":
		rates = githubLinuxArm64
	}
	for _, r := range rates {
		if cpu <= r.cores {
			return r.perMinute
		}
	}
	last := rates[len(rates)-1]
	return last.perMinute * float64(cpu) / float64(last.cores)
}

// machineCores returns how many vCPUs a job saw: what the instance type has,
// or a MicroVM's peak, which is four times its baseline. The recorded CPU is
// the fallback: it is the label's minimum, not what a pinned type= has.
func machineCores(instanceType string, recorded int) int {
	if v, ok := strings.CutPrefix(instanceType, "microvm-"); ok {
		if n, err := strconv.Atoi(strings.TrimSuffix(v, "vcpu")); err == nil {
			return 4 * n
		}
		return recorded
	}
	_, size, ok := strings.Cut(instanceType, ".")
	if !ok {
		return recorded
	}
	switch size {
	case "medium":
		return 1
	case "large":
		return 2
	case "xlarge":
		return 4
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(size, "xlarge")); err == nil {
		return 4 * n
	}
	return recorded
}

// Summarize aggregates a set of jobs.
func Summarize(us []Usage) Summary {
	s := Summary{ByRepo: map[string]float64{}, ByWorkflow: map[string]float64{}, ByType: map[string]float64{}}
	var waits []float64
	spot, ran := 0, 0
	for _, u := range us {
		s.Jobs++
		if u.Status == "failed" {
			s.LaunchFailures++
		}
		s.Cost += u.Cost
		if u.Billed {
			s.Billed += u.Cost
		} else {
			s.Estimated += u.Cost
		}
		s.ByRepo[u.Repo] += u.Cost
		s.ByWorkflow[u.Repo+" / "+u.Workflow] += u.Cost
		if u.InstanceType != "" {
			s.ByType[u.InstanceType] += u.Cost
		}
		if u.StartedAt > 0 && u.CompletedAt > u.StartedAt {
			d := time.Duration(u.CompletedAt-u.StartedAt) * time.Second
			s.Minutes += d.Minutes()
			// GitHub bills each job rounded up to the whole minute.
			s.GitHubCost += math.Ceil(d.Minutes()) * GitHubPerMinute(machineCores(u.InstanceType, u.CPU), u.Platform, archOf(u.InstanceType))
			ran++
			if u.Lifecycle == "spot" {
				spot++
			}
		}
		if u.StartedAt > 0 && u.QueuedAt > 0 && u.StartedAt >= u.QueuedAt {
			waits = append(waits, float64(u.StartedAt-u.QueuedAt))
		}
	}
	if ran > 0 {
		s.SpotShare = float64(spot) / float64(ran)
	}
	if len(waits) > 0 {
		sort.Float64s(waits)
		s.MedianWait = waits[len(waits)/2]
	}
	return s
}

// archOf guesses the architecture from an instance type (Graviton types
// carry a "g" after the generation digit, e.g. c7g, m8g, c7gd). MicroVMs run
// on Graviton.
func archOf(instanceType string) string {
	if strings.HasPrefix(instanceType, "microvm-") {
		return "arm64"
	}
	for i := 0; i < len(instanceType); i++ {
		c := instanceType[i]
		if c >= '0' && c <= '9' {
			if i+1 < len(instanceType) && instanceType[i+1] == 'g' {
				return "arm64"
			}
			return "x64"
		}
	}
	return "x64"
}
