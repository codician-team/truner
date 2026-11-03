package config

import (
	"fmt"
	"sort"
)

// Default values applied before any user configuration.
const (
	DefaultRunner      = "2cpu-linux-x64"
	DefaultImage       = "ubuntu24"
	DefaultDiskGiB     = 40
	DefaultTimeoutMin  = 720
	DefaultIdleMin     = 15
	DefaultMaxInstance = 100
	// RAMPerCPU is the GiB of memory per vCPU assumed when none is given,
	// matching the shape of GitHub-hosted runners.
	RAMPerCPU = 4
)

// BuiltinDefaults is the bottom configuration layer.
func BuiltinDefaults() RunnerSpec {
	return RunnerSpec{
		CPU:         &IntRange{Min: 2, Max: 2, Single: true},
		Arch:        ArchX64,
		Image:       DefaultImage,
		Spot:        SpotAuto,
		Disk:        DefaultDiskGiB,
		Cache:       CacheS3,
		Timeout:     DefaultTimeoutMin,
		IdleTimeout: DefaultIdleMin,
		Private:     boolPtr(false),
		SSH:         boolPtr(false),
	}
}

const (
	ssmUbuntu24  = "/aws/service/canonical/ubuntu/server/24.04/stable/current/{arch}/hvm/ebs-gp3/ami-id"
	ssmUbuntu22  = "/aws/service/canonical/ubuntu/server/22.04/stable/current/{arch}/hvm/ebs-gp2/ami-id"
	ssmDLAMI24   = "/aws/service/deeplearning/ami/{march}/base-oss-nvidia-driver-gpu-ubuntu-24.04/latest/ami-id"
	ssmDLAMI22   = "/aws/service/deeplearning/ami/{march}/base-oss-nvidia-driver-gpu-ubuntu-22.04/latest/ami-id"
	ssmWindows22 = "/aws/service/ami-windows-latest/Windows_Server-2022-English-Full-Base"
	ssmWindows25 = "/aws/service/ami-windows-latest/Windows_Server-2025-English-Full-Base"
)

var builtinImages = map[string]ImageSpec{
	"ubuntu24":      {Platform: PlatformLinux, Archs: StringList{ArchX64, ArchARM64}, SSMParameter: ssmUbuntu24, Profile: "standard", Bake: boolPtr(true), OS: "ubuntu24"},
	"ubuntu24-full": {Platform: PlatformLinux, Archs: StringList{ArchX64, ArchARM64}, SSMParameter: ssmUbuntu24, Profile: "full", Bake: boolPtr(true), OS: "ubuntu24"},
	"ubuntu22":      {Platform: PlatformLinux, Archs: StringList{ArchX64, ArchARM64}, SSMParameter: ssmUbuntu22, Profile: "standard", Bake: boolPtr(true), OS: "ubuntu22"},
	"ubuntu22-full": {Platform: PlatformLinux, Archs: StringList{ArchX64, ArchARM64}, SSMParameter: ssmUbuntu22, Profile: "full", Bake: boolPtr(true), OS: "ubuntu22"},
	"ubuntu24-gpu":  {Platform: PlatformLinux, Archs: StringList{ArchX64}, SSMParameter: ssmDLAMI24, Profile: "standard", Bake: boolPtr(false), OS: "ubuntu24"},
	"ubuntu22-gpu":  {Platform: PlatformLinux, Archs: StringList{ArchX64}, SSMParameter: ssmDLAMI22, Profile: "standard", Bake: boolPtr(false), OS: "ubuntu22"},
	"windows2022":   {Platform: PlatformWindows, Archs: StringList{ArchX64}, SSMParameter: ssmWindows22, Profile: "standard", Bake: boolPtr(false), OS: "win22"},
	"windows2025":   {Platform: PlatformWindows, Archs: StringList{ArchX64}, SSMParameter: ssmWindows25, Profile: "standard", Bake: boolPtr(false), OS: "win25"},
}

var imageAliases = map[string]string{
	"ubuntu-latest":  "ubuntu24",
	"ubuntu-24.04":   "ubuntu24",
	"ubuntu-22.04":   "ubuntu22",
	"ubuntu":         "ubuntu24",
	"windows-latest": "windows2025",
	"windows-2025":   "windows2025",
	"windows-2022":   "windows2022",
	"windows":        "windows2025",
	"gpu":            "ubuntu24-gpu",
}

var builtinRunners = func() map[string]RunnerSpec {
	m := map[string]RunnerSpec{}
	for _, cpu := range []int{2, 4, 8, 16, 32, 48, 64, 96} {
		for _, arch := range []string{ArchX64, ArchARM64} {
			m[fmt.Sprintf("%dcpu-linux-%s", cpu, arch)] = RunnerSpec{
				CPU:  &IntRange{Min: cpu, Max: cpu, Single: true},
				RAM:  &IntRange{Min: cpu * RAMPerCPU, Single: true},
				Arch: arch,
			}
		}
	}
	for _, cpu := range []int{2, 4, 8, 16, 32, 64} {
		m[fmt.Sprintf("%dcpu-windows-x64", cpu)] = RunnerSpec{
			CPU:   &IntRange{Min: cpu, Max: cpu, Single: true},
			RAM:   &IntRange{Min: cpu * RAMPerCPU, Single: true},
			Arch:  ArchX64,
			Image: "windows2025",
			Disk:  80,
		}
	}
	m["gpu-nvidia"] = RunnerSpec{
		CPU:   &IntRange{Min: 4},
		RAM:   &IntRange{Min: 16, Single: true},
		GPU:   1,
		Arch:  ArchX64,
		Image: "ubuntu24-gpu",
		Disk:  120,
	}
	m["gpu-nvidia-large"] = RunnerSpec{
		CPU:   &IntRange{Min: 16},
		RAM:   &IntRange{Min: 64, Single: true},
		GPU:   1,
		Arch:  ArchX64,
		Image: "ubuntu24-gpu",
		Disk:  200,
	}
	return m
}()

// BuiltinRunnerNames lists the built-in runner profiles, sorted.
func BuiltinRunnerNames() []string {
	var out []string
	for k := range builtinRunners {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// BuiltinRunner returns a built-in runner profile.
func BuiltinRunner(name string) (RunnerSpec, bool) {
	r, ok := builtinRunners[name]
	return r, ok
}

// BuiltinImageNames lists the built-in images, sorted.
func BuiltinImageNames() []string {
	var out []string
	for k := range builtinImages {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// BuiltinImage returns a built-in image (aliases are resolved).
func BuiltinImage(name string) (string, ImageSpec, bool) {
	if a, ok := imageAliases[name]; ok {
		name = a
	}
	img, ok := builtinImages[name]
	return name, img, ok
}
