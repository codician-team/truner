package config

import (
	"errors"
	"fmt"
)

// Micro machines are Lambda MicroVMs: they resume from a snapshot in seconds,
// which suits short checks and gates, but they are ARM64 only, have at most
// 4 vCPU at baseline (bursting to 4x) and have no Docker.

// MicroImage is the image name micro jobs report.
const MicroImage = "micro"

// MicroMaxMinutes is the longest a MicroVM may exist.
const MicroMaxMinutes = 480

// microSizes are the baseline vCPUs truner builds MicroVM images for. Memory
// is 2 GiB per vCPU, and both burst to 4x the baseline.
var microSizes = []int{1, 2, 4}

// MicroDiskGiB is the disk of a MicroVM with this baseline vCPU count.
func MicroDiskGiB(vcpu int) int {
	switch {
	case vcpu >= 4:
		return 32
	case vcpu >= 2:
		return 16
	}
	return 8
}

// toMicro turns a resolved spec into a MicroVM spec, or explains why the job
// needs an EC2 machine.
func toMicro(js *JobSpec, spec RunnerSpec) error {
	switch {
	case js.Platform() == PlatformWindows:
		return errors.New("micro machines run Linux; remove micro for Windows jobs")
	case js.GPU > 0:
		return errors.New("micro machines have no GPU; remove micro or gpu")
	case js.NVMe:
		return errors.New("micro machines have no local NVMe; remove micro or nvme")
	case js.Private:
		return errors.New("micro machines cannot use private subnets; remove micro or private")
	case js.SSH:
		return errors.New("micro machines do not support ssh; debug on an EC2 machine instead")
	case js.Pool != "":
		return errors.New("micro machines start in seconds without a pool; remove micro or pool")
	case len(js.Families) > 0 || len(js.Types) > 0:
		return errors.New("micro machines come in fixed sizes; remove family and type, and use cpu=1, 2 or 4")
	}
	vcpu := 0
	for _, v := range microSizes {
		if v >= js.CPU.Min && (spec.RAM == nil || spec.RAM.Min <= 8*v) {
			vcpu = v
			break
		}
	}
	if vcpu == 0 {
		return fmt.Errorf("micro machines have at most 4 vCPU and 8 GiB at baseline (16 vCPU and 32 GiB at peak); remove micro for cpu=%s", js.CPU.String())
	}
	js.Micro = true
	js.MicroMemoryMiB = vcpu * 2048
	js.CPU = IntRange{Min: vcpu, Max: vcpu, Single: true}
	js.RAM = IntRange{Min: 2 * vcpu, Max: 8 * vcpu}
	js.Arch = ArchARM64
	js.Disk = MicroDiskGiB(vcpu)
	js.DiskIOPS, js.DiskThroughput, js.DiskInitRate = 0, 0, 0
	js.Spot = SpotOff
	if js.Image != DefaultImage && js.Image != MicroImage {
		js.Warnings = append(js.Warnings, "image "+js.Image+" ignored: micro machines use their own image")
	}
	js.Image = MicroImage
	js.ImageSpec = ImageSpec{Platform: PlatformLinux, OS: "ubuntu24", Profile: "none"}
	if js.Keep > 0 {
		js.Warnings = append(js.Warnings, "keep ignored: micro machines end with their job")
		js.Keep = 0
	}
	if js.Timeout > MicroMaxMinutes {
		js.Timeout = MicroMaxMinutes
	}
	return nil
}

// WithoutMicro is the EC2 machine a micro job runs on when no MicroVM is
// available: the same vCPU count on Graviton with the default image.
func (s *JobSpec) WithoutMicro(reason string) *JobSpec {
	c := *s
	c.Micro, c.MicroMemoryMiB = false, 0
	c.RAM = IntRange{Min: s.CPU.Min * RAMPerCPU}
	c.Spot = SpotAuto
	c.Disk = DefaultDiskGiB
	c.Image, c.ImageSpec, _ = BuiltinImage(DefaultImage)
	if c.ImageSpec.Platform == "" {
		c.ImageSpec.Platform = PlatformLinux
	}
	c.Warnings = append(append([]string(nil), s.Warnings...), "this micro job runs on an EC2 machine: "+reason)
	return &c
}
