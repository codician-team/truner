package controlplane

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/codician-team/truner/internal/config"
)

// Tag keys.
const (
	TagManaged    = "truner:managed"
	TagStack      = "truner:stack"
	TagRole       = "truner:role" // runner | builder | pool
	TagJobID      = "truner:job-id"
	TagRunnerName = "truner:runner-name"
	TagRepo       = "truner:repository"
	TagWorkflow   = "truner:workflow"
	TagDeadline   = "truner:deadline"
	TagImage      = "truner:image"
	TagArch       = "truner:arch"
	TagBakeID     = "truner:bake-id"
	TagPool       = "truner:pool"
	TagPoolID     = "truner:pool-id"
	TagPoolState  = "truner:pool-state"
	TagPoolHash   = "truner:pool-hash"
	TagVersion    = "truner:version"
)

// LaunchInput describes one machine to start.
type LaunchInput struct {
	Name             string
	Spec             *config.JobSpec
	Image            *ResolvedImage
	UserData         string
	Subnets          []string
	Tags             map[string]string
	ShutdownBehavior ec2types.ShutdownBehavior
	ForceOnDemand    bool
}

// LaunchResult is the outcome of a successful launch.
type LaunchResult struct {
	InstanceID   string
	InstanceType string
	Lifecycle    string // spot | on-demand
	AZ           string
	SubnetID     string
	FellBack     bool
	RunnerID     int64 // set when the runner was registered during the launch (pools)
	Took         time.Duration
}

// CapacityError means no capacity was available; the launch should be retried.
type CapacityError struct{ Msg string }

func (e *CapacityError) Error() string { return "no capacity: " + e.Msg }

// Launch creates an ephemeral launch template and starts one instance with
// EC2 Fleet, trying spot first (price-capacity-optimized across every
// matching instance type and AZ) and falling back to on-demand.
func (cp *ControlPlane) Launch(ctx context.Context, in LaunchInput) (*LaunchResult, error) {
	start := time.Now()
	if len(in.Subnets) == 0 {
		return nil, errors.New("no subnets configured")
	}
	if in.ShutdownBehavior == "" {
		in.ShutdownBehavior = ec2types.ShutdownBehaviorTerminate
	}
	lt, err := cp.createLaunchTemplate(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("create launch template: %w", err)
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := cp.EC2.DeleteLaunchTemplate(dctx, &ec2.DeleteLaunchTemplateInput{LaunchTemplateId: lt}); err != nil {
			cp.Log.Warn("delete launch template failed (janitor will retry)", "lt", aws.ToString(lt), "err", err)
		}
	}()

	overrides := buildOverrides(in.Spec, in.Subnets)
	var attempts []ec2types.DefaultTargetCapacityType
	if !in.ForceOnDemand && in.Spec.Spot != config.SpotOff {
		attempts = append(attempts, ec2types.DefaultTargetCapacityTypeSpot)
	}
	if in.Spec.Spot != config.SpotOnly || in.ForceOnDemand {
		attempts = append(attempts, ec2types.DefaultTargetCapacityTypeOnDemand)
	}
	var lastErr error
	for i, kind := range attempts {
		res, err := cp.createFleet(ctx, lt, overrides, kind, in.Tags)
		if err == nil {
			res.FellBack = i > 0 && kind == ec2types.DefaultTargetCapacityTypeOnDemand
			res.Took = time.Since(start)
			return res, nil
		}
		lastErr = err
		var ce *CapacityError
		if !errors.As(err, &ce) {
			return nil, err
		}
		cp.Log.Warn("launch attempt failed", "kind", kind, "err", err)
	}
	return nil, lastErr
}

func (cp *ControlPlane) createLaunchTemplate(ctx context.Context, in LaunchInput) (*string, error) {
	img := in.Image
	disk := in.Spec.Disk
	if img.RootSizeGiB > disk {
		disk = img.RootSizeGiB
	}
	ebs := &ec2types.LaunchTemplateEbsBlockDeviceRequest{
		VolumeSize:          aws.Int32(int32(disk)),
		VolumeType:          ec2types.VolumeTypeGp3,
		DeleteOnTermination: aws.Bool(true),
		Encrypted:           aws.Bool(true),
	}
	if in.Spec.DiskIOPS > 0 {
		ebs.Iops = aws.Int32(int32(in.Spec.DiskIOPS))
	}
	if in.Spec.DiskThroughput > 0 {
		ebs.Throughput = aws.Int32(int32(in.Spec.DiskThroughput))
	}
	if in.Spec.DiskInitRate > 0 {
		ebs.VolumeInitializationRate = aws.Int32(int32(in.Spec.DiskInitRate))
	}
	var tags []ec2types.Tag
	keys := make([]string, 0, len(in.Tags))
	for k := range in.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		tags = append(tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(truncate(in.Tags[k], 255))})
	}
	data := &ec2types.RequestLaunchTemplateData{
		ImageId:                           aws.String(img.ID),
		IamInstanceProfile:                &ec2types.LaunchTemplateIamInstanceProfileSpecificationRequest{Arn: aws.String(cp.Env.InstanceProfileArn)},
		SecurityGroupIds:                  []string{cp.Env.SecurityGroup},
		InstanceInitiatedShutdownBehavior: in.ShutdownBehavior,
		UserData:                          aws.String(base64.StdEncoding.EncodeToString([]byte(in.UserData))),
		BlockDeviceMappings:               []ec2types.LaunchTemplateBlockDeviceMappingRequest{{DeviceName: aws.String(img.RootDevice), Ebs: ebs}},
		MetadataOptions: &ec2types.LaunchTemplateInstanceMetadataOptionsRequest{
			HttpEndpoint:            ec2types.LaunchTemplateInstanceMetadataEndpointStateEnabled,
			HttpTokens:              ec2types.LaunchTemplateHttpTokensStateRequired,
			HttpPutResponseHopLimit: aws.Int32(2), // containers may use the instance role
			InstanceMetadataTags:    ec2types.LaunchTemplateInstanceMetadataTagsStateEnabled,
		},
		TagSpecifications: []ec2types.LaunchTemplateTagSpecificationRequest{
			{ResourceType: ec2types.ResourceTypeInstance, Tags: tags},
			{ResourceType: ec2types.ResourceTypeVolume, Tags: tags},
		},
	}
	out, err := cp.EC2.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String(truncate("truner-"+in.Name, 128)),
		LaunchTemplateData: data,
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeLaunchTemplate,
			Tags: []ec2types.Tag{
				{Key: aws.String(TagManaged), Value: aws.String("true")},
				{Key: aws.String(TagStack), Value: aws.String(cp.Env.Stack)},
			},
		}},
	})
	if err != nil {
		return nil, err
	}
	return out.LaunchTemplate.LaunchTemplateId, nil
}

// buildOverrides expands a spec into EC2 Fleet overrides, one per subnet
// (attribute-based selection) or per subnet × instance type.
func buildOverrides(spec *config.JobSpec, subnets []string) []ec2types.FleetLaunchTemplateOverridesRequest {
	var out []ec2types.FleetLaunchTemplateOverridesRequest
	if len(spec.Types) > 0 {
		for _, t := range spec.Types {
			for _, s := range subnets {
				if len(out) >= 300 {
					return out
				}
				out = append(out, ec2types.FleetLaunchTemplateOverridesRequest{SubnetId: aws.String(s), InstanceType: ec2types.InstanceType(t)})
			}
		}
		return out
	}
	req := InstanceRequirements(spec)
	for _, s := range subnets {
		out = append(out, ec2types.FleetLaunchTemplateOverridesRequest{SubnetId: aws.String(s), InstanceRequirements: req})
	}
	return out
}

// hddFamilies have hard disk instance storage. They are cheap on spot, but
// their disks take minutes to set up and build slower than EBS, so they are
// only used when a job names their family. (LocalStorageTypes=ssd would also
// drop every type without instance storage.)
var hddFamilies = []string{"d2.*", "d3.*", "d3en.*", "h1.*"}

// oldFamilies are generations from 2017 to 2019 (and x1 from 2016) that EC2
// still lists as current. They are often the cheapest spot pools, and jobs ran
// two to three times as long on them: at the median, a frontend build took
// 345 s on r5a.2xlarge and 148 s on c7a.2xlarge. Like hddFamilies, they are
// only used when a job names their family. The wildcards cover the variants
// (m5* is m5, m5a, m5ad, m5d, m5dn, m5n and m5zn).
var oldFamilies = []string{"m5*", "c5*", "r5*", "z1d*", "i3*", "x1*"}

// ExcludedFamilies are the instance types a job without family= or type=
// never gets.
func ExcludedFamilies() []string { return append(slices.Clone(hddFamilies), oldFamilies...) }

// InstanceRequirements maps a spec to EC2 attribute-based instance selection.
func InstanceRequirements(spec *config.JobSpec) *ec2types.InstanceRequirementsRequest {
	req := &ec2types.InstanceRequirementsRequest{
		VCpuCount:           &ec2types.VCpuCountRangeRequest{Min: aws.Int32(int32(spec.CPU.Min))},
		MemoryMiB:           &ec2types.MemoryMiBRequest{Min: aws.Int32(int32(spec.RAM.Min * 1024))},
		InstanceGenerations: []ec2types.InstanceGeneration{ec2types.InstanceGenerationCurrent},
		BareMetal:           ec2types.BareMetalExcluded,
		LocalStorage:        ec2types.LocalStorageIncluded,
	}
	if spec.CPU.Max > 0 {
		req.VCpuCount.Max = aws.Int32(int32(spec.CPU.Max))
	}
	if spec.RAM.Max > 0 {
		req.MemoryMiB.Max = aws.Int32(int32(spec.RAM.Max * 1024))
	}
	if spec.Arch == config.ArchARM64 {
		req.CpuManufacturers = []ec2types.CpuManufacturer{ec2types.CpuManufacturerAmazonWebServices}
	} else {
		req.CpuManufacturers = []ec2types.CpuManufacturer{ec2types.CpuManufacturerIntel, ec2types.CpuManufacturerAmd}
	}
	if len(spec.Families) > 0 {
		req.AllowedInstanceTypes = spec.Families
		req.BurstablePerformance = ec2types.BurstablePerformanceIncluded
	} else {
		req.BurstablePerformance = ec2types.BurstablePerformanceExcluded
		req.ExcludedInstanceTypes = ExcludedFamilies()
	}
	if spec.NVMe {
		req.LocalStorage = ec2types.LocalStorageRequired
		req.LocalStorageTypes = []ec2types.LocalStorageType{ec2types.LocalStorageTypeSsd}
	}
	if spec.GPU > 0 {
		req.AcceleratorCount = &ec2types.AcceleratorCountRequest{Min: aws.Int32(int32(spec.GPU))}
		req.AcceleratorTypes = []ec2types.AcceleratorType{ec2types.AcceleratorTypeGpu}
		req.AcceleratorManufacturers = []ec2types.AcceleratorManufacturer{ec2types.AcceleratorManufacturerNvidia}
	} else {
		req.AcceleratorCount = &ec2types.AcceleratorCountRequest{Max: aws.Int32(0)}
	}
	return req
}

func (cp *ControlPlane) createFleet(ctx context.Context, lt *string, overrides []ec2types.FleetLaunchTemplateOverridesRequest, kind ec2types.DefaultTargetCapacityType, tags map[string]string) (*LaunchResult, error) {
	in := &ec2.CreateFleetInput{
		Type: ec2types.FleetTypeInstant,
		LaunchTemplateConfigs: []ec2types.FleetLaunchTemplateConfigRequest{{
			LaunchTemplateSpecification: &ec2types.FleetLaunchTemplateSpecificationRequest{LaunchTemplateId: lt, Version: aws.String("$Latest")},
			Overrides:                   overrides,
		}},
		TargetCapacitySpecification: &ec2types.TargetCapacitySpecificationRequest{
			TotalTargetCapacity:       aws.Int32(1),
			DefaultTargetCapacityType: kind,
		},
		SpotOptions: &ec2types.SpotOptionsRequest{
			AllocationStrategy:           ec2types.SpotAllocationStrategyPriceCapacityOptimized,
			InstanceInterruptionBehavior: ec2types.SpotInstanceInterruptionBehaviorTerminate,
		},
		OnDemandOptions: &ec2types.OnDemandOptionsRequest{AllocationStrategy: ec2types.FleetOnDemandAllocationStrategyLowestPrice},
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeFleet,
			Tags: []ec2types.Tag{
				{Key: aws.String(TagManaged), Value: aws.String("true")},
				{Key: aws.String(TagStack), Value: aws.String(cp.Env.Stack)},
				{Key: aws.String(TagRunnerName), Value: aws.String(tags[TagRunnerName])},
			},
		}},
	}
	out, err := cp.EC2.CreateFleet(ctx, in)
	if err != nil {
		msg := err.Error()
		for _, c := range []string{"InsufficientInstanceCapacity", "Unsupported", "RequestLimitExceeded", "InternalError", "ServiceUnavailable"} {
			if strings.Contains(msg, c) {
				return nil, &CapacityError{Msg: msg}
			}
		}
		return nil, err
	}
	for _, inst := range out.Instances {
		if len(inst.InstanceIds) == 0 {
			continue
		}
		r := &LaunchResult{
			InstanceID:   inst.InstanceIds[0],
			InstanceType: string(inst.InstanceType),
			Lifecycle:    "on-demand",
		}
		if inst.Lifecycle == ec2types.InstanceLifecycleSpot {
			r.Lifecycle = "spot"
		}
		if inst.LaunchTemplateAndOverrides != nil && inst.LaunchTemplateAndOverrides.Overrides != nil {
			o := inst.LaunchTemplateAndOverrides.Overrides
			r.AZ = aws.ToString(o.AvailabilityZone)
			r.SubnetID = aws.ToString(o.SubnetId)
		}
		return r, nil
	}
	var msgs []string
	fatal := false
	seen := map[string]bool{}
	for _, e := range out.Errors {
		code := aws.ToString(e.ErrorCode)
		m := code + ": " + aws.ToString(e.ErrorMessage)
		if !seen[m] {
			seen[m] = true
			msgs = append(msgs, m)
		}
		if strings.HasPrefix(code, "InvalidParameter") || strings.HasPrefix(code, "InvalidLaunchTemplate") || code == "UnauthorizedOperation" || code == "InvalidAMIID.Malformed" || code == "InvalidAMIID.NotFound" {
			fatal = true
		}
	}
	if len(msgs) == 0 {
		msgs = append(msgs, "fleet returned no instances")
	}
	if len(msgs) > 5 {
		msgs = msgs[:5]
	}
	msg := strings.Join(msgs, "; ")
	if fatal {
		return nil, errors.New(msg)
	}
	return nil, &CapacityError{Msg: string(kind) + ": " + msg}
}

// TerminateInstances terminates instances and MicroVMs, ignoring
// already-gone ones.
func (cp *ControlPlane) TerminateInstances(ctx context.Context, ids ...string) error {
	var ec2IDs, micro []string
	for _, id := range ids {
		if isMicroVM(id) {
			micro = append(micro, id)
		} else {
			ec2IDs = append(ec2IDs, id)
		}
	}
	err := cp.terminateMicrovms(ctx, micro...)
	if len(ec2IDs) == 0 {
		return err
	}
	_, eerr := cp.EC2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ec2IDs})
	if eerr != nil && strings.Contains(eerr.Error(), "InvalidInstanceID.NotFound") {
		eerr = nil
	}
	return errors.Join(err, eerr)
}

// ManagedInstances lists this stack's instances in the given states.
func (cp *ControlPlane) ManagedInstances(ctx context.Context, states ...string) ([]ec2types.Instance, error) {
	if len(states) == 0 {
		states = []string{"pending", "running", "stopping", "stopped", "shutting-down"}
	}
	out := []ec2types.Instance{} // nil only on errors
	p := ec2.NewDescribeInstancesPaginator(cp.EC2, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag:" + TagStack), Values: []string{cp.Env.Stack}},
		{Name: aws.String("instance-state-name"), Values: states},
	}})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range page.Reservations {
			out = append(out, r.Instances...)
		}
	}
	return out, nil
}

// InstanceTag returns a tag value of an instance.
func InstanceTag(i ec2types.Instance, key string) string {
	for _, t := range i.Tags {
		if aws.ToString(t.Key) == key {
			return aws.ToString(t.Value)
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
