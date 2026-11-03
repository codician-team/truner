// Package config defines truner's configuration model: runner profiles,
// images, pools and account-wide settings, plus the logic that resolves a
// job's labels into a concrete JobSpec.
//
// Configuration is layered (later layers win):
//
//	built-in defaults → global config (S3) → repo config (.github/truner.yml)
//	→ runner profile → job labels
package config

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Architectures.
const (
	ArchX64   = "x64"
	ArchARM64 = "arm64"
)

// Platforms.
const (
	PlatformLinux   = "linux"
	PlatformWindows = "windows"
)

// Cache backends.
const (
	CacheS3     = "s3"     // truner magic cache (S3-backed, transparent)
	CacheGitHub = "github" // GitHub's hosted cache service
	CacheOff    = "off"
)

// SpotMode controls the purchasing option.
type SpotMode string

const (
	SpotAuto SpotMode = "auto" // spot, falling back to on-demand
	SpotOnly SpotMode = "only" // spot only, never on-demand
	SpotOff  SpotMode = "off"  // on-demand only
)

// ParseSpotMode accepts booleans and mode names.
func ParseSpotMode(v string) (SpotMode, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "true", "yes", "on", "1", "auto", "spot", "fallback":
		return SpotAuto, nil
	case "only", "strict", "spot-only":
		return SpotOnly, nil
	case "false", "no", "off", "0", "on-demand", "ondemand", "od":
		return SpotOff, nil
	}
	return "", fmt.Errorf("invalid spot mode %q (use true, false or only)", v)
}

// UnmarshalYAML accepts `spot: true`, `spot: false` and `spot: only`.
func (s *SpotMode) UnmarshalYAML(n *yaml.Node) error {
	m, err := ParseSpotMode(n.Value)
	if err != nil {
		return err
	}
	*s = m
	return nil
}

// IntRange is an inclusive integer range. Max == 0 means unbounded.
// Single is set when the range was written as a single number.
type IntRange struct {
	Min, Max int
	Single   bool
}

// ParseIntRange parses "4", "4-8", "4:8" or "4+".
func ParseIntRange(s string) (IntRange, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return IntRange{}, fmt.Errorf("empty range")
	}
	if strings.HasSuffix(s, "+") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "+"))
		if err != nil || n < 0 {
			return IntRange{}, fmt.Errorf("invalid range %q", s)
		}
		return IntRange{Min: n}, nil
	}
	for _, sep := range []string{"-", ":", ".."} {
		if a, b, ok := strings.Cut(s, sep); ok {
			lo, err1 := strconv.Atoi(strings.TrimSpace(a))
			hi, err2 := strconv.Atoi(strings.TrimSpace(b))
			if err1 != nil || err2 != nil || lo < 0 || hi < lo {
				return IntRange{}, fmt.Errorf("invalid range %q", s)
			}
			return IntRange{Min: lo, Max: hi}, nil
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return IntRange{}, fmt.Errorf("invalid number %q", s)
	}
	return IntRange{Min: n, Max: n, Single: true}, nil
}

// UnmarshalYAML accepts `4`, `"4-8"` and `[4, 8]`.
func (r *IntRange) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		v, err := ParseIntRange(n.Value)
		if err != nil {
			return err
		}
		*r = v
		return nil
	case yaml.SequenceNode:
		var xs []int
		if err := n.Decode(&xs); err != nil {
			return err
		}
		if len(xs) != 2 || xs[0] < 0 || xs[1] < xs[0] {
			return fmt.Errorf("range must be [min, max]")
		}
		*r = IntRange{Min: xs[0], Max: xs[1]}
		return nil
	}
	return fmt.Errorf("invalid range")
}

func (r IntRange) String() string {
	switch {
	case r.Single || r.Min == r.Max:
		return strconv.Itoa(r.Min)
	case r.Max == 0:
		return fmt.Sprintf("%d+", r.Min)
	}
	return fmt.Sprintf("%d-%d", r.Min, r.Max)
}

// StringList accepts either a YAML list or a "a+b" / "a,b" string.
type StringList []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (l *StringList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var out []string
		for _, p := range strings.FieldsFunc(n.Value, func(r rune) bool { return r == '+' || r == ',' || r == ' ' }) {
			out = append(out, p)
		}
		*l = out
		return nil
	case yaml.SequenceNode:
		var xs []string
		if err := n.Decode(&xs); err != nil {
			return err
		}
		*l = xs
		return nil
	}
	return fmt.Errorf("expected list")
}

// RunnerSpec describes the machine a job runs on. Zero values mean "unset"
// so specs can be layered.
type RunnerSpec struct {
	// Extends names another runner profile to inherit from.
	Extends string `yaml:"extends,omitempty" json:"extends,omitempty"`

	CPU    *IntRange  `yaml:"cpu,omitempty" json:"cpu,omitempty"`
	RAM    *IntRange  `yaml:"ram,omitempty" json:"ram,omitempty"` // GiB
	Family StringList `yaml:"family,omitempty" json:"family,omitempty"`
	Type   StringList `yaml:"type,omitempty" json:"type,omitempty"`
	Arch   string     `yaml:"arch,omitempty" json:"arch,omitempty"`
	GPU    int        `yaml:"gpu,omitempty" json:"gpu,omitempty"`
	NVMe   *bool      `yaml:"nvme,omitempty" json:"nvme,omitempty"`

	Image string   `yaml:"image,omitempty" json:"image,omitempty"`
	Spot  SpotMode `yaml:"spot,omitempty" json:"spot,omitempty"`

	Disk           int `yaml:"disk,omitempty" json:"disk,omitempty"` // GiB
	DiskIOPS       int `yaml:"diskIops,omitempty" json:"diskIops,omitempty"`
	DiskThroughput int `yaml:"diskThroughput,omitempty" json:"diskThroughput,omitempty"` // MiB/s
	DiskInitRate   int `yaml:"diskInitRate,omitempty" json:"diskInitRate,omitempty"`     // MiB/s, EBS provisioned initialization

	Private *bool  `yaml:"private,omitempty" json:"private,omitempty"`
	SSH     *bool  `yaml:"ssh,omitempty" json:"ssh,omitempty"`
	Cache   string `yaml:"cache,omitempty" json:"cache,omitempty"`

	Timeout     int `yaml:"timeout,omitempty" json:"timeout,omitempty"`         // minutes, max instance lifetime
	IdleTimeout int `yaml:"idleTimeout,omitempty" json:"idleTimeout,omitempty"` // minutes to wait for a job
	Keep        int `yaml:"keep,omitempty" json:"keep,omitempty"`               // minutes to keep the machine after the job (debugging)

	Pool string            `yaml:"pool,omitempty" json:"pool,omitempty"`
	Env  map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Tags map[string]string `yaml:"tags,omitempty" json:"tags,omitempty"`
}

// ImageSpec describes how to find (or build) the AMI for an image name.
type ImageSpec struct {
	Platform string     `yaml:"platform,omitempty" json:"platform,omitempty"` // linux | windows
	Archs    StringList `yaml:"archs,omitempty" json:"archs,omitempty"`       // supported architectures; empty = any

	// Exactly one source should be set. SSM parameters may contain {arch}
	// (amd64|arm64) and {march} (x86_64|arm64) placeholders.
	AMI          string `yaml:"ami,omitempty" json:"ami,omitempty"`
	Name         string `yaml:"name,omitempty" json:"name,omitempty"`   // AMI name filter, wildcards allowed
	Owner        string `yaml:"owner,omitempty" json:"owner,omitempty"` // AMI owner for name filter
	SSMParameter string `yaml:"ssm,omitempty" json:"ssm,omitempty"`

	// Profile is the software set the agent installs when the image is not
	// pre-baked: none | standard | full.
	Profile string `yaml:"profile,omitempty" json:"profile,omitempty"`
	// Bake builds and maintains an AMI with the profile preinstalled.
	Bake *bool `yaml:"bake,omitempty" json:"bake,omitempty"`
	// OS is exported to jobs as ImageOS (used by setup-* actions).
	OS string `yaml:"os,omitempty" json:"os,omitempty"`
	// User is the account jobs run as (default "runner").
	User string `yaml:"user,omitempty" json:"user,omitempty"`
}

// PoolSpec keeps pre-provisioned, stopped instances ready for instant start.
type PoolSpec struct {
	Runner      string     `yaml:"runner,omitempty" json:"runner,omitempty"`
	Size        int        `yaml:"size" json:"size"`
	MaxAgeHours int        `yaml:"maxAgeHours,omitempty" json:"maxAgeHours,omitempty"`
	Spec        RunnerSpec `yaml:",inline" json:"spec"`
}

// Access restricts which repositories may use runners.
type Access struct {
	Repos StringList `yaml:"repos,omitempty" json:"repos,omitempty"` // glob allow-list ("org/*"); empty = all
	Deny  StringList `yaml:"deny,omitempty" json:"deny,omitempty"`
}

// Limits are safety rails against runaway spend.
type Limits struct {
	MaxInstances     int     `yaml:"maxInstances,omitempty" json:"maxInstances,omitempty"`
	MonthlyBudgetUSD float64 `yaml:"monthlyBudgetUsd,omitempty" json:"monthlyBudgetUsd,omitempty"` // hard stop
}

// Global is the account-wide configuration stored in S3 (config/truner.yml).
type Global struct {
	Defaults RunnerSpec            `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	Runners  map[string]RunnerSpec `yaml:"runners,omitempty" json:"runners,omitempty"`
	Images   map[string]ImageSpec  `yaml:"images,omitempty" json:"images,omitempty"`
	Pools    map[string]PoolSpec   `yaml:"pools,omitempty" json:"pools,omitempty"`
	Access   Access                `yaml:"access,omitempty" json:"access,omitempty"`
	Limits   Limits                `yaml:"limits,omitempty" json:"limits,omitempty"`

	// AutoBake builds optimized AMIs for images that are used (default true).
	AutoBake *bool `yaml:"autoBake,omitempty" json:"autoBake,omitempty"`
	// BakeIntervalDays rebuilds baked images this often (default 7).
	BakeIntervalDays int `yaml:"bakeIntervalDays,omitempty" json:"bakeIntervalDays,omitempty"`
	// RetryOnInterruption re-runs jobs killed by a spot interruption (default true).
	RetryOnInterruption *bool `yaml:"retryOnInterruption,omitempty" json:"retryOnInterruption,omitempty"`
}

// Repo is the per-repository configuration in .github/truner.yml.
type Repo struct {
	Defaults RunnerSpec            `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	Runners  map[string]RunnerSpec `yaml:"runners,omitempty" json:"runners,omitempty"`
	Images   map[string]ImageSpec  `yaml:"images,omitempty" json:"images,omitempty"`
}

// ParseGlobal parses and validates a global config document.
func ParseGlobal(data []byte) (*Global, error) {
	g := &Global{}
	if len(bytes.TrimSpace(data)) == 0 {
		return g, nil
	}
	if err := strictUnmarshal(data, g); err != nil {
		return nil, err
	}
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return g, nil
}

// ParseRepo parses and validates a repository config document.
func ParseRepo(data []byte) (*Repo, error) {
	r := &Repo{}
	if len(bytes.TrimSpace(data)) == 0 {
		return r, nil
	}
	if err := strictUnmarshal(data, r); err != nil {
		return nil, err
	}
	if err := validateRunnerMap(r.Runners); err != nil {
		return nil, err
	}
	if err := r.Defaults.Validate(); err != nil {
		return nil, fmt.Errorf("defaults: %w", err)
	}
	if err := validateImageMap(r.Images); err != nil {
		return nil, err
	}
	return r, nil
}

func strictUnmarshal(data []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	return nil
}

// Validate checks a global config for errors.
func (g *Global) Validate() error {
	if err := g.Defaults.Validate(); err != nil {
		return fmt.Errorf("defaults: %w", err)
	}
	if err := validateRunnerMap(g.Runners); err != nil {
		return err
	}
	if err := validateImageMap(g.Images); err != nil {
		return err
	}
	for name, p := range g.Pools {
		if !validName(name) {
			return fmt.Errorf("pools: invalid pool name %q", name)
		}
		if p.Size < 0 || p.Size > 100 {
			return fmt.Errorf("pools.%s: size must be between 0 and 100", name)
		}
		if err := p.Spec.Validate(); err != nil {
			return fmt.Errorf("pools.%s: %w", name, err)
		}
	}
	if g.Limits.MaxInstances < 0 {
		return fmt.Errorf("limits.maxInstances must be positive")
	}
	if g.Limits.MonthlyBudgetUSD < 0 {
		return fmt.Errorf("limits.monthlyBudgetUsd must be positive")
	}
	return nil
}

func validateRunnerMap(m map[string]RunnerSpec) error {
	for name, r := range m {
		if !validName(name) {
			return fmt.Errorf("runners: invalid runner name %q", name)
		}
		if err := r.Validate(); err != nil {
			return fmt.Errorf("runners.%s: %w", name, err)
		}
	}
	return nil
}

func validateImageMap(m map[string]ImageSpec) error {
	for name, img := range m {
		if !validName(name) {
			return fmt.Errorf("images: invalid image name %q", name)
		}
		if err := img.Validate(); err != nil {
			return fmt.Errorf("images.%s: %w", name, err)
		}
	}
	return nil
}

func validName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// Validate checks a runner spec for errors.
func (r RunnerSpec) Validate() error {
	if r.CPU != nil && (r.CPU.Min < 1 || r.CPU.Min > 896) {
		return fmt.Errorf("cpu must be between 1 and 896")
	}
	if r.RAM != nil && r.RAM.Min > 32768 {
		return fmt.Errorf("ram is in GiB and must be at most 32768")
	}
	if r.Arch != "" {
		if _, err := NormalizeArch(r.Arch); err != nil {
			return err
		}
	}
	if r.GPU < 0 || r.GPU > 16 {
		return fmt.Errorf("gpu must be between 0 and 16")
	}
	if r.Disk != 0 && (r.Disk < 8 || r.Disk > 16384) {
		return fmt.Errorf("disk must be between 8 and 16384 GiB")
	}
	if r.DiskIOPS != 0 && (r.DiskIOPS < 3000 || r.DiskIOPS > 80000) {
		return fmt.Errorf("diskIops must be between 3000 and 80000")
	}
	if r.DiskThroughput != 0 && (r.DiskThroughput < 125 || r.DiskThroughput > 2000) {
		return fmt.Errorf("diskThroughput must be between 125 and 2000 MiB/s")
	}
	if r.DiskInitRate != 0 && (r.DiskInitRate < 100 || r.DiskInitRate > 300) {
		return fmt.Errorf("diskInitRate must be between 100 and 300 MiB/s")
	}
	switch r.Cache {
	case "", CacheS3, CacheGitHub, CacheOff:
	default:
		return fmt.Errorf("cache must be s3, github or off")
	}
	if r.Timeout < 0 || r.Timeout > 7200 {
		return fmt.Errorf("timeout must be between 1 and 7200 minutes")
	}
	if r.IdleTimeout < 0 || r.IdleTimeout > 1440 {
		return fmt.Errorf("idleTimeout must be between 1 and 1440 minutes")
	}
	if r.Keep < 0 || r.Keep > 720 {
		return fmt.Errorf("keep must be between 0 and 720 minutes")
	}
	for _, t := range append(append([]string{}, r.Family...), r.Type...) {
		if !validInstancePattern(t) {
			return fmt.Errorf("invalid instance type or family %q", t)
		}
	}
	for k := range r.Env {
		if !validEnvName(k) {
			return fmt.Errorf("invalid env var name %q", k)
		}
	}
	return nil
}

// Validate checks an image spec for errors.
func (i ImageSpec) Validate() error {
	switch i.Platform {
	case "", PlatformLinux, PlatformWindows:
	default:
		return fmt.Errorf("platform must be linux or windows")
	}
	n := 0
	for _, s := range []string{i.AMI, i.Name, i.SSMParameter} {
		if s != "" {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("exactly one of ami, name or ssm must be set")
	}
	if i.AMI != "" && !strings.HasPrefix(i.AMI, "ami-") {
		return fmt.Errorf("ami must look like ami-0123456789abcdef0")
	}
	switch i.Profile {
	case "", "none", "standard", "full":
	default:
		return fmt.Errorf("profile must be none, standard or full")
	}
	for _, a := range i.Archs {
		if _, err := NormalizeArch(a); err != nil {
			return err
		}
	}
	return nil
}

func validInstancePattern(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '*') {
			return false
		}
	}
	return true
}

func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// NormalizeArch maps architecture spellings to x64 or arm64.
func NormalizeArch(a string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(a)) {
	case "x64", "amd64", "x86_64", "x86-64", "intel", "amd":
		return ArchX64, nil
	case "arm64", "aarch64", "arm", "graviton":
		return ArchARM64, nil
	}
	return "", fmt.Errorf("invalid arch %q (use x64 or arm64)", a)
}

func boolPtr(b bool) *bool { return &b }
