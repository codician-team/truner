// Package costs summarizes job usage and compares it with GitHub-hosted runners.
package costs

import (
	"math"
	"sort"
	"time"
)

// Usage is one job's billing-relevant facts.
type Usage struct {
	Repo, Workflow, Status  string
	InstanceType, Lifecycle string
	Platform                string
	CPU                     int
	Cost                    float64
	QueuedAt, StartedAt     int64
	CompletedAt             int64
}

// Summary aggregates usage.
type Summary struct {
	Jobs           int
	LaunchFailures int
	Minutes        float64
	Cost           float64
	GitHubCost     float64
	SpotShare      float64
	MedianWait     float64 // seconds from queued to started
	ByRepo         map[string]float64
	ByWorkflow     map[string]float64
	ByType         map[string]float64
}

// GitHubPerMinute estimates the list price of a GitHub-hosted runner minute
// of the same size. Prices are approximate public list prices for larger
// runners and are only used for comparisons.
func GitHubPerMinute(cpu int, platform, arch string) float64 {
	if cpu < 2 {
		cpu = 2
	}
	perCore := 0.003 // Linux x64
	switch {
	case platform == "windows":
		perCore = 0.006
	case arch == "arm64":
		perCore = 0.0025
	}
	return perCore * float64(cpu)
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
		s.ByRepo[u.Repo] += u.Cost
		s.ByWorkflow[u.Repo+" / "+u.Workflow] += u.Cost
		if u.InstanceType != "" {
			s.ByType[u.InstanceType] += u.Cost
		}
		if u.StartedAt > 0 && u.CompletedAt > u.StartedAt {
			d := time.Duration(u.CompletedAt-u.StartedAt) * time.Second
			s.Minutes += d.Minutes()
			// GitHub bills each job rounded up to the whole minute.
			s.GitHubCost += math.Ceil(d.Minutes()) * GitHubPerMinute(u.CPU, u.Platform, archOf(u.InstanceType))
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
// carry a "g" after the generation digit, e.g. c7g, m8g, c7gd).
func archOf(instanceType string) string {
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
