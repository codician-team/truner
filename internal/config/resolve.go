package config

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codician-team/truner/internal/labels"
)

// JobSpec is the fully resolved description of the machine for one job.
type JobSpec struct {
	Runner string `json:"runner,omitempty"`

	CPU      IntRange `json:"cpu"`
	RAM      IntRange `json:"ram"` // GiB, Max 0 = unbounded
	Families []string `json:"families,omitempty"`
	Types    []string `json:"types,omitempty"`
	Arch     string   `json:"arch"`
	GPU      int      `json:"gpu,omitempty"`
	NVMe     bool     `json:"nvme,omitempty"`

	Image     string    `json:"image"`
	ImageSpec ImageSpec `json:"imageSpec"`
	Spot      SpotMode  `json:"spot"`

	Disk           int `json:"disk"`
	DiskIOPS       int `json:"diskIops,omitempty"`
	DiskThroughput int `json:"diskThroughput,omitempty"`
	DiskInitRate   int `json:"diskInitRate,omitempty"`

	Private bool   `json:"private,omitempty"`
	SSH     bool   `json:"ssh,omitempty"`
	Cache   string `json:"cache"`

	Timeout     int `json:"timeout"`
	IdleTimeout int `json:"idleTimeout"`
	Keep        int `json:"keep,omitempty"`

	Pool string            `json:"pool,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	Tags map[string]string `json:"tags,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
}

// Platform returns linux or windows.
func (s *JobSpec) Platform() string {
	if s.ImageSpec.Platform == "" {
		return PlatformLinux
	}
	return s.ImageSpec.Platform
}

// Summary is a short human readable description, e.g. "4 vCPU / 16+ GiB x64, spot".
func (s *JobSpec) Summary() string {
	var b strings.Builder
	if len(s.Types) > 0 {
		b.WriteString(strings.Join(s.Types, "|"))
	} else {
		fmt.Fprintf(&b, "%s vCPU", s.CPU.String())
		if s.RAM.Min > 0 || s.RAM.Max > 0 {
			fmt.Fprintf(&b, " / %s GiB", s.RAM.String())
		}
		if len(s.Families) > 0 {
			fmt.Fprintf(&b, " [%s]", strings.Join(s.Families, "|"))
		}
	}
	fmt.Fprintf(&b, " %s %s", s.Platform(), s.Arch)
	if s.GPU > 0 {
		fmt.Fprintf(&b, ", %d GPU", s.GPU)
	}
	switch s.Spot {
	case SpotOff:
		b.WriteString(", on-demand")
	case SpotOnly:
		b.WriteString(", spot only")
	default:
		b.WriteString(", spot")
	}
	return b.String()
}

// Merge overlays every set field of src onto dst.
func Merge(dst *RunnerSpec, src RunnerSpec) {
	if src.CPU != nil {
		v := *src.CPU
		dst.CPU = &v
	}
	if src.RAM != nil {
		v := *src.RAM
		dst.RAM = &v
	}
	if src.Family != nil {
		dst.Family = append(StringList(nil), src.Family...)
	}
	if src.Type != nil {
		dst.Type = append(StringList(nil), src.Type...)
	}
	if src.Arch != "" {
		dst.Arch = src.Arch
	}
	if src.GPU != 0 {
		dst.GPU = src.GPU
	}
	if src.NVMe != nil {
		dst.NVMe = boolPtr(*src.NVMe)
	}
	if src.Image != "" {
		dst.Image = src.Image
	}
	if src.Spot != "" {
		dst.Spot = src.Spot
	}
	if src.Disk != 0 {
		dst.Disk = src.Disk
	}
	if src.DiskIOPS != 0 {
		dst.DiskIOPS = src.DiskIOPS
	}
	if src.DiskThroughput != 0 {
		dst.DiskThroughput = src.DiskThroughput
	}
	if src.DiskInitRate != 0 {
		dst.DiskInitRate = src.DiskInitRate
	}
	if src.Private != nil {
		dst.Private = boolPtr(*src.Private)
	}
	if src.SSH != nil {
		dst.SSH = boolPtr(*src.SSH)
	}
	if src.Cache != "" {
		dst.Cache = src.Cache
	}
	if src.Timeout != 0 {
		dst.Timeout = src.Timeout
	}
	if src.IdleTimeout != 0 {
		dst.IdleTimeout = src.IdleTimeout
	}
	if src.Keep != 0 {
		dst.Keep = src.Keep
	}
	if src.Pool != "" {
		dst.Pool = src.Pool
	}
	if len(src.Env) > 0 {
		if dst.Env == nil {
			dst.Env = map[string]string{}
		}
		for k, v := range src.Env {
			dst.Env[k] = v
		}
	}
	if len(src.Tags) > 0 {
		if dst.Tags == nil {
			dst.Tags = map[string]string{}
		}
		for k, v := range src.Tags {
			dst.Tags[k] = v
		}
	}
}

// Resolver resolves names against the repo, global and built-in layers.
type Resolver struct {
	Global *Global
	Repo   *Repo
}

// LookupRunner finds a runner profile by name and expands `extends`.
func (r Resolver) LookupRunner(name string) (RunnerSpec, error) {
	return r.lookupRunner(name, map[string]bool{})
}

func (r Resolver) lookupRunner(name string, seen map[string]bool) (RunnerSpec, error) {
	if seen[name] {
		return RunnerSpec{}, fmt.Errorf("runner %q: extends cycle", name)
	}
	seen[name] = true
	var spec RunnerSpec
	var found bool
	if r.Repo != nil {
		spec, found = r.Repo.Runners[name]
	}
	if !found && r.Global != nil {
		spec, found = r.Global.Runners[name]
	}
	if !found {
		spec, found = builtinRunners[name]
	}
	if !found {
		return RunnerSpec{}, fmt.Errorf("unknown runner %q (built-ins: %s, ...)", name, strings.Join(BuiltinRunnerNames()[:4], ", "))
	}
	if spec.Extends == "" {
		return spec, nil
	}
	base, err := r.lookupRunner(spec.Extends, seen)
	if err != nil {
		return RunnerSpec{}, err
	}
	Merge(&base, spec)
	base.Extends = ""
	return base, nil
}

// LookupImage finds an image by name. Names may carry an architecture
// suffix ("ubuntu24-arm64"), which is returned separately.
func (r Resolver) LookupImage(name string) (canonical string, img ImageSpec, archSuffix string, err error) {
	if strings.HasPrefix(name, "ami-") {
		return name, ImageSpec{AMI: name, Profile: "none"}, "", nil
	}
	try := func(n string) (string, ImageSpec, bool) {
		if r.Repo != nil {
			if img, ok := r.Repo.Images[n]; ok {
				return n, img, true
			}
		}
		if r.Global != nil {
			if img, ok := r.Global.Images[n]; ok {
				return n, img, true
			}
		}
		return BuiltinImage(n)
	}
	if c, img, ok := try(name); ok {
		return c, img, "", nil
	}
	for _, suffix := range []string{"-x64", "-arm64", "-amd64", "-aarch64"} {
		if strings.HasSuffix(name, suffix) {
			if c, img, ok := try(strings.TrimSuffix(name, suffix)); ok {
				a, _ := NormalizeArch(strings.TrimPrefix(suffix, "-"))
				return c, img, a, nil
			}
		}
	}
	return "", ImageSpec{}, "", fmt.Errorf("unknown image %q (built-ins: %s)", name, strings.Join(BuiltinImageNames(), ", "))
}

// Resolve turns job labels into a JobSpec using the configuration layers.
func (r Resolver) Resolve(l labels.Labels) (*JobSpec, error) {
	spec := BuiltinDefaults()
	if r.Global != nil {
		Merge(&spec, r.Global.Defaults)
	}
	if r.Repo != nil {
		Merge(&spec, r.Repo.Defaults)
	}
	fromLabels, runnerName, warnings, err := FromLabels(l.Values)
	if err != nil {
		return nil, err
	}
	for _, k := range l.Unknown {
		warnings = append(warnings, fmt.Sprintf("ignored unknown label key %q", k))
	}

	if fromLabels.Pool != "" {
		pool, err := r.ResolvePool(fromLabels.Pool)
		if err != nil {
			return nil, err
		}
		// Hardware is fixed by the pool; only behavioral settings may vary.
		for _, k := range []string{"cpu", "ram", "family", "type", "arch", "image", "spot", "disk", "gpu", "nvme", "private"} {
			if _, ok := l.Values[k]; ok {
				warnings = append(warnings, fmt.Sprintf("label %q ignored: hardware is defined by pool %q", k, fromLabels.Pool))
			}
		}
		if fromLabels.SSH != nil {
			pool.SSH = *fromLabels.SSH
		}
		if fromLabels.Keep != 0 {
			pool.Keep = fromLabels.Keep
		}
		if fromLabels.Timeout != 0 {
			pool.Timeout = fromLabels.Timeout
		}
		if fromLabels.Cache != "" {
			pool.Cache = fromLabels.Cache
		}
		pool.Warnings = append(pool.Warnings, warnings...)
		return pool, nil
	}

	if runnerName != "" {
		prof, err := r.LookupRunner(runnerName)
		if err != nil {
			return nil, err
		}
		Merge(&spec, prof)
	}
	Merge(&spec, fromLabels)
	js, err := r.finalize(spec, runnerName)
	if err != nil {
		return nil, err
	}
	js.Warnings = append(warnings, js.Warnings...)
	return js, nil
}

// ResolvePool resolves the spec instances of a pool are created with.
func (r Resolver) ResolvePool(name string) (*JobSpec, error) {
	if r.Global == nil {
		return nil, fmt.Errorf("unknown pool %q", name)
	}
	p, ok := r.Global.Pools[name]
	if !ok {
		return nil, fmt.Errorf("unknown pool %q", name)
	}
	spec := BuiltinDefaults()
	Merge(&spec, r.Global.Defaults)
	// Pools are account-wide, so repo-level runners do not apply.
	gr := Resolver{Global: r.Global}
	if p.Runner != "" {
		prof, err := gr.LookupRunner(p.Runner)
		if err != nil {
			return nil, fmt.Errorf("pool %q: %w", name, err)
		}
		Merge(&spec, prof)
	}
	Merge(&spec, p.Spec)
	// Pool machines are stopped between uses, which spot does not allow.
	spec.Spot = SpotOff
	js, err := gr.finalize(spec, p.Runner)
	if err != nil {
		return nil, fmt.Errorf("pool %q: %w", name, err)
	}
	js.Pool = name
	return js, nil
}

func (r Resolver) finalize(spec RunnerSpec, runnerName string) (*JobSpec, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	js := &JobSpec{
		Runner:         runnerName,
		Families:       expandFamilies(spec.Family),
		Types:          append([]string(nil), spec.Type...),
		GPU:            spec.GPU,
		Spot:           spec.Spot,
		Disk:           spec.Disk,
		DiskIOPS:       spec.DiskIOPS,
		DiskThroughput: spec.DiskThroughput,
		DiskInitRate:   spec.DiskInitRate,
		Cache:          spec.Cache,
		Timeout:        spec.Timeout,
		IdleTimeout:    spec.IdleTimeout,
		Keep:           spec.Keep,
		Pool:           spec.Pool,
		Env:            spec.Env,
		Tags:           spec.Tags,
	}
	if spec.NVMe != nil {
		js.NVMe = *spec.NVMe
	}
	if spec.Private != nil {
		js.Private = *spec.Private
	}
	if spec.SSH != nil {
		js.SSH = *spec.SSH
	}
	if spec.CPU != nil {
		js.CPU = *spec.CPU
	}
	if js.CPU.Min == 0 {
		js.CPU = IntRange{Min: 2, Max: 2, Single: true}
	}
	switch {
	case spec.RAM != nil:
		js.RAM = *spec.RAM
		if js.RAM.Single {
			// "ram=16" means "at least 16 GiB".
			js.RAM.Max, js.RAM.Single = 0, false
		}
	case len(js.Families) > 0 || len(js.Types) > 0:
		// Let the chosen families decide the memory ratio.
		js.RAM = IntRange{}
	default:
		js.RAM = IntRange{Min: js.CPU.Min * RAMPerCPU}
	}

	imgName, img, archSuffix, err := r.LookupImage(spec.Image)
	if err != nil {
		return nil, err
	}
	js.Image, js.ImageSpec = imgName, img
	if js.ImageSpec.Platform == "" {
		js.ImageSpec.Platform = PlatformLinux
	}
	arch, err := NormalizeArch(spec.Arch)
	if err != nil {
		return nil, err
	}
	if archSuffix != "" {
		arch = archSuffix
	}
	if len(img.Archs) == 1 {
		only, _ := NormalizeArch(img.Archs[0])
		if spec.Arch != "" && spec.Arch != ArchX64 && arch != only {
			return nil, fmt.Errorf("image %q only supports %s", imgName, only)
		}
		arch = only
	} else if len(img.Archs) > 1 {
		ok := false
		for _, a := range img.Archs {
			if n, _ := NormalizeArch(a); n == arch {
				ok = true
			}
		}
		if !ok {
			return nil, fmt.Errorf("image %q does not support %s", imgName, arch)
		}
	}
	js.Arch = arch
	if js.Platform() == PlatformWindows && arch != ArchX64 {
		return nil, fmt.Errorf("windows runners are x64 only")
	}
	if js.Platform() == PlatformWindows && js.Disk < 50 {
		js.Disk = 50
		js.Warnings = append(js.Warnings, "disk raised to 50 GiB (Windows minimum)")
	}
	if js.GPU > 0 && js.Arch == ArchARM64 && len(js.Types) == 0 && len(js.Families) == 0 {
		js.Families = []string{"g5g*"}
	}
	if js.Spot == "" {
		js.Spot = SpotAuto
	}
	if js.Cache == "" {
		js.Cache = CacheS3
	}
	if js.Timeout == 0 {
		js.Timeout = DefaultTimeoutMin
	}
	if js.IdleTimeout == 0 {
		js.IdleTimeout = DefaultIdleMin
	}
	return js, nil
}

func expandFamilies(fams []string) []string {
	var out []string
	for _, f := range fams {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" {
			continue
		}
		if !strings.Contains(f, ".") && !strings.HasSuffix(f, "*") {
			f += "*"
		}
		out = append(out, f)
	}
	return out
}

// FromLabels converts label key/values to a RunnerSpec. It returns the
// requested runner profile name separately.
func FromLabels(v map[string]string) (spec RunnerSpec, runner string, warnings []string, err error) {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		val := v[k]
		bad := func(e error) error { return fmt.Errorf("label %s=%s: %w", k, val, e) }
		switch k {
		case "runner":
			runner = val
		case "cpu":
			r, e := ParseIntRange(val)
			if e != nil {
				return spec, "", nil, bad(e)
			}
			spec.CPU = &r
		case "ram":
			r, e := ParseIntRange(strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(val), "gb"), "g"))
			if e != nil {
				return spec, "", nil, bad(e)
			}
			spec.RAM = &r
		case "family":
			spec.Family = labels.SplitList(val)
		case "type":
			spec.Type = labels.SplitList(val)
		case "arch":
			a, e := NormalizeArch(val)
			if e != nil {
				return spec, "", nil, bad(e)
			}
			spec.Arch = a
		case "image":
			spec.Image = val
		case "spot":
			m, e := ParseSpotMode(val)
			if e != nil {
				return spec, "", nil, bad(e)
			}
			spec.Spot = m
		case "disk", "disk-iops", "disk-throughput", "disk-init":
			n, e := strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(val), "gb"), "g"))
			if e != nil || n <= 0 {
				return spec, "", nil, bad(fmt.Errorf("must be a positive number"))
			}
			switch k {
			case "disk":
				spec.Disk = n
			case "disk-iops":
				spec.DiskIOPS = n
			case "disk-throughput":
				spec.DiskThroughput = n
			case "disk-init":
				spec.DiskInitRate = n
			}
		case "gpu":
			if b, e := labels.ParseBool(val); e == nil && b && val != "1" {
				spec.GPU = 1
				continue
			}
			n, e := strconv.Atoi(val)
			if e != nil || n < 0 {
				return spec, "", nil, bad(fmt.Errorf("must be a GPU count"))
			}
			spec.GPU = n
		case "nvme", "private", "ssh":
			b, e := labels.ParseBool(val)
			if e != nil {
				return spec, "", nil, bad(e)
			}
			switch k {
			case "nvme":
				spec.NVMe = &b
			case "private":
				spec.Private = &b
			case "ssh":
				spec.SSH = &b
			}
		case "cache":
			switch strings.ToLower(val) {
			case "", "true", "yes", "on", "s3", "magic":
				spec.Cache = CacheS3
			case "github", "gh", "native":
				spec.Cache = CacheGitHub
			case "false", "no", "off", "none":
				spec.Cache = CacheOff
			default:
				return spec, "", nil, bad(fmt.Errorf("use s3, github or off"))
			}
		case "timeout", "idle-timeout", "keep":
			n, e := parseMinutes(val)
			if e != nil {
				return spec, "", nil, bad(e)
			}
			switch k {
			case "timeout":
				spec.Timeout = n
			case "idle-timeout":
				spec.IdleTimeout = n
			case "keep":
				spec.Keep = n
			}
		case "pool":
			spec.Pool = val
		case "run":
			// uniqueness token, intentionally ignored
		default:
			warnings = append(warnings, fmt.Sprintf("ignored label key %q", k))
		}
	}
	return spec, runner, warnings, nil
}

// parseMinutes parses "30", "30m", "2h" or "1h30m" into minutes.
func parseMinutes(s string) (int, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if b, err := labels.ParseBool(s); err == nil && b && s != "1" {
		return 30, nil // "keep=true" → 30 minutes
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("must be positive")
		}
		return n, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return int(d.Minutes()), nil
}

// RepoAllowed reports whether owner/repo may use truner under these rules.
func (a Access) RepoAllowed(fullName string) bool {
	fullName = strings.ToLower(fullName)
	for _, d := range a.Deny {
		if ok, _ := path.Match(strings.ToLower(d), fullName); ok {
			return false
		}
	}
	if len(a.Repos) == 0 {
		return true
	}
	for _, p := range a.Repos {
		if ok, _ := path.Match(strings.ToLower(p), fullName); ok {
			return true
		}
	}
	return false
}

// BoolOr dereferences b, returning def when nil.
func BoolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}
