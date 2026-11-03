// Package labels parses the `runs-on:` labels of a GitHub Actions job into
// truner runner settings.
//
// Two equivalent syntaxes are supported:
//
//	runs-on: truner/cpu=4/ram=16/run=${{ github.run_id }}
//	runs-on: [truner, cpu=4, ram=16, "run=${{ github.run_id }}"]
//
// The first label may also carry a value, e.g. `truner=${{ github.run_id }}/cpu=4`.
package labels

import (
	"fmt"
	"sort"
	"strings"
)

// DefaultPrefix is the label that routes a job to truner.
const DefaultPrefix = "truner"

// Labels is the parsed form of a job's labels.
type Labels struct {
	// Raw holds the labels exactly as GitHub sent them. The runner must be
	// registered with exactly these labels for GitHub to route the job to it.
	Raw []string
	// Values holds normalized key=value settings.
	Values map[string]string
	// Unknown lists keys that are not recognized (typos, mostly).
	Unknown []string
}

// aliases maps accepted spellings to canonical keys.
var aliases = map[string]string{
	"runner":          "runner",
	"profile":         "runner",
	"cpu":             "cpu",
	"cpus":            "cpu",
	"vcpu":            "cpu",
	"vcpus":           "cpu",
	"ram":             "ram",
	"mem":             "ram",
	"memory":          "ram",
	"family":          "family",
	"families":        "family",
	"type":            "type",
	"types":           "type",
	"instance-type":   "type",
	"arch":            "arch",
	"architecture":    "arch",
	"image":           "image",
	"ami":             "image",
	"spot":            "spot",
	"disk":            "disk",
	"disk-size":       "disk",
	"volume":          "disk",
	"iops":            "disk-iops",
	"disk-iops":       "disk-iops",
	"throughput":      "disk-throughput",
	"disk-throughput": "disk-throughput",
	"disk-init":       "disk-init",
	"gpu":             "gpu",
	"gpus":            "gpu",
	"nvme":            "nvme",
	"micro":           "micro",
	"microvm":         "micro",
	"private":         "private",
	"static-ip":       "private",
	"ssh":             "ssh",
	"cache":           "cache",
	"timeout":         "timeout",
	"idle-timeout":    "idle-timeout",
	"keep":            "keep",
	"debug":           "keep",
	"pool":            "pool",
	"run":             "run",
	"run-id":          "run",
	"id":              "run",
}

// IsKnownKey reports whether key (or an alias of it) is a recognized setting.
func IsKnownKey(key string) bool {
	_, ok := aliases[strings.ToLower(strings.TrimSpace(key))]
	return ok
}

// Match reports whether any of the labels targets truner with the given prefix.
func Match(prefix string, raw []string) bool {
	for _, l := range raw {
		l = strings.TrimSpace(l)
		if l == prefix || strings.HasPrefix(l, prefix+"/") || strings.HasPrefix(l, prefix+"=") {
			return true
		}
	}
	return false
}

// Parse parses job labels. ok is false when the job is not meant for truner.
func Parse(prefix string, raw []string) (Labels, bool) {
	out := Labels{Raw: append([]string(nil), raw...), Values: map[string]string{}}
	if prefix == "" {
		prefix = DefaultPrefix
	}
	if !Match(prefix, raw) {
		return out, false
	}
	unknown := map[string]bool{}
	add := func(seg string) {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			return
		}
		k, v, found := strings.Cut(seg, "=")
		if !found {
			// Bare words like "self-hosted" or "linux" are ignored; a bare
			// known key such as "spot" or "ssh" is treated as "=true".
			k = strings.ToLower(strings.TrimSpace(k))
			if canon, ok := aliases[k]; ok && isBoolKey(canon) {
				out.Values[canon] = "true"
			}
			return
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		canon, ok := aliases[k]
		if !ok {
			unknown[k] = true
			return
		}
		out.Values[canon] = v
	}
	for _, l := range raw {
		l = strings.TrimSpace(l)
		switch {
		case l == prefix:
			continue
		case strings.HasPrefix(l, prefix+"/"):
			for _, seg := range strings.Split(strings.TrimPrefix(l, prefix+"/"), "/") {
				add(seg)
			}
		case strings.HasPrefix(l, prefix+"="):
			segs := strings.Split(strings.TrimPrefix(l, prefix+"="), "/")
			// The first segment is a free-form uniqueness token.
			out.Values["run"] = segs[0]
			for _, seg := range segs[1:] {
				add(seg)
			}
		default:
			// key=value labels, or bare boolean switches such as "spot"
			// (other bare words like "self-hosted" are ignored by add).
			add(l)
		}
	}
	for k := range unknown {
		out.Unknown = append(out.Unknown, k)
	}
	sort.Strings(out.Unknown)
	return out, true
}

func isBoolKey(k string) bool {
	switch k {
	case "spot", "nvme", "private", "ssh", "cache", "micro":
		return true
	}
	return false
}

// SplitList splits list values written as "a+b+c" or "a,b,c".
func SplitList(v string) []string {
	f := func(r rune) bool { return r == '+' || r == ',' || r == ' ' }
	var out []string
	for _, p := range strings.FieldsFunc(v, f) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ParseBool parses permissive boolean label values.
func ParseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "1", "true", "yes", "on", "y":
		return true, nil
	case "0", "false", "no", "off", "n":
		return false, nil
	}
	return false, fmt.Errorf("invalid boolean %q", v)
}
