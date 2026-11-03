package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/truner/truner/internal/agentcfg"
	"github.com/truner/truner/internal/config"
	"github.com/truner/truner/internal/github"
)

// Pool instance states (tag truner:pool-state).
const (
	poolPreparing = "preparing"
	poolReady     = "ready"
	poolClaimed   = "claimed"
)

// poolHash fingerprints everything that makes pool machines interchangeable,
// so changing a pool's config recycles its machines.
func (cp *ControlPlane) poolHash(spec *config.JobSpec) string {
	b, _ := json.Marshal(struct {
		S *config.JobSpec
		V string
	}{spec, cp.Env.Version})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// claimPoolInstance starts a ready pool machine for a job. It returns nil
// when the pool has nothing ready.
func (cp *ControlPlane) claimPoolInstance(ctx context.Context, gh *github.Client, job *Job, spec *config.JobSpec, runnerName string, rel *runnerRelease) (*LaunchResult, error) {
	start := time.Now()
	// Match on the pool's own spec: job-level settings such as keep or ssh
	// are applied at claim time and must not affect which machines fit.
	pure, err := config.Resolver{Global: cp.GlobalConfig(ctx)}.ResolvePool(spec.Pool)
	if err != nil {
		return nil, err
	}
	out, err := cp.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag:" + TagStack), Values: []string{cp.Env.Stack}},
		{Name: aws.String("tag:" + TagPool), Values: []string{spec.Pool}},
		{Name: aws.String("tag:" + TagPoolState), Values: []string{poolReady}},
		{Name: aws.String("tag:" + TagPoolHash), Values: []string{cp.poolHash(pure)}},
		{Name: aws.String("instance-state-name"), Values: []string{"stopped"}},
	}})
	if err != nil {
		return nil, err
	}
	var cands []ec2types.Instance
	for _, r := range out.Reservations {
		cands = append(cands, r.Instances...)
	}
	sort.Slice(cands, func(i, j int) bool { return aws.ToTime(cands[i].LaunchTime).Before(aws.ToTime(cands[j].LaunchTime)) })
	for _, inst := range cands {
		id := aws.ToString(inst.InstanceId)
		if ok, err := cp.Store.PutItem(ctx, "claim#"+id, Item{"jobId": job.JobID}, 24*time.Hour, true); err != nil || !ok {
			continue
		}
		jit, err := gh.GenerateJITConfig(ctx, job.InstallationID, job.Owner, job.Repo, runnerName, job.Labels)
		if err != nil {
			cp.Store.DeleteItem(ctx, "claim#"+id)
			return nil, err
		}
		img := &ResolvedImage{ID: aws.ToString(inst.ImageId), Name: spec.Image, Baked: true}
		ac := cp.agentConfig(ctx, job, spec, img, runnerName, jit.EncodedJITConfig, rel, "")
		ud, err := cp.buildUserData(ctx, ac)
		if err == nil {
			_, err = cp.EC2.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{InstanceId: inst.InstanceId, UserData: &ec2types.SecureBlobAttributeValue{Value: []byte(ud)}})
		}
		if err == nil {
			_, err = cp.EC2.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{InstanceId: inst.InstanceId, InstanceInitiatedShutdownBehavior: &ec2types.AttributeValue{Value: aws.String("terminate")}})
		}
		if err == nil {
			tags := cp.runnerTags(job, spec, runnerName)
			tags[TagPoolState] = poolClaimed
			var t []ec2types.Tag
			for k, v := range tags {
				t = append(t, ec2types.Tag{Key: aws.String(k), Value: aws.String(truncate(v, 255))})
			}
			_, err = cp.EC2.CreateTags(ctx, &ec2.CreateTagsInput{Resources: []string{id}, Tags: t})
		}
		if err == nil {
			_, err = cp.EC2.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{id}})
		}
		if err != nil {
			gh.DeleteRunner(ctx, job.InstallationID, job.Owner, job.Repo, jit.Runner.ID)
			cp.Log.Warn("pool claim failed, discarding machine", "instance", id, "err", err)
			cp.TerminateInstances(ctx, id)
			continue
		}
		az := ""
		if inst.Placement != nil {
			az = aws.ToString(inst.Placement.AvailabilityZone)
		}
		cp.emit("PoolHits", 1, UnitCount)
		return &LaunchResult{InstanceID: id, InstanceType: string(inst.InstanceType), Lifecycle: "on-demand", AZ: az,
			SubnetID: aws.ToString(inst.SubnetId), RunnerID: jit.Runner.ID, Took: time.Since(start)}, nil
	}
	cp.emit("PoolMisses", 1, UnitCount)
	return nil, nil
}

// ReconcilePools keeps every pool at its configured size and recycles stale machines.
func (cp *ControlPlane) ReconcilePools(ctx context.Context, instances []ec2types.Instance) error {
	g := cp.GlobalConfig(ctx)
	res := config.Resolver{Global: g}
	byPool := map[string][]ec2types.Instance{}
	for _, inst := range instances {
		if p := InstanceTag(inst, TagPool); p != "" && InstanceTag(inst, TagPoolState) != poolClaimed {
			byPool[p] = append(byPool[p], inst)
		}
	}
	// Machines of deleted pools.
	for name, insts := range byPool {
		if _, ok := g.Pools[name]; !ok {
			for _, inst := range insts {
				cp.TerminateInstances(ctx, aws.ToString(inst.InstanceId))
			}
		}
	}
	var firstErr error
	for name, p := range g.Pools {
		spec, err := res.ResolvePool(name)
		if err != nil {
			cp.Log.Error("invalid pool", "pool", name, "err", err)
			continue
		}
		if spec.Platform() != config.PlatformLinux {
			cp.Log.Error("pools support Linux images only", "pool", name)
			continue
		}
		hash := cp.poolHash(spec)
		maxAge := time.Duration(p.MaxAgeHours) * time.Hour
		if maxAge == 0 {
			maxAge = 7 * 24 * time.Hour
		}
		healthy := 0
		var ready []string
		for _, inst := range byPool[name] {
			id := aws.ToString(inst.InstanceId)
			state := inst.State.Name
			pstate := InstanceTag(inst, TagPoolState)
			age := time.Since(aws.ToTime(inst.LaunchTime))
			if state == ec2types.InstanceStateNameShuttingDown || state == ec2types.InstanceStateNameTerminated {
				continue
			}
			stale := InstanceTag(inst, TagPoolHash) != hash || age > maxAge
			switch {
			case stale && state == ec2types.InstanceStateNameStopped:
				cp.Log.Info("recycling stale pool machine", "pool", name, "instance", id)
				cp.TerminateInstances(ctx, id)
			case pstate == poolPreparing && state == ec2types.InstanceStateNameStopped:
				var m bakeMarker
				ok, err := cp.readMarker(ctx, "pools/"+InstanceTag(inst, TagPoolID)+".json", &m)
				if err == nil && ok && m.OK {
					cp.EC2.CreateTags(ctx, &ec2.CreateTagsInput{Resources: []string{id}, Tags: []ec2types.Tag{{Key: aws.String(TagPoolState), Value: aws.String(poolReady)}}})
					healthy++
				} else {
					cp.Log.Warn("pool machine failed to prepare", "pool", name, "instance", id, "err", m.Error)
					cp.TerminateInstances(ctx, id)
				}
			case pstate == poolPreparing && age > 45*time.Minute:
				cp.TerminateInstances(ctx, id)
			default:
				healthy++
				if pstate == poolReady && state == ec2types.InstanceStateNameStopped {
					ready = append(ready, id)
				}
			}
		}
		// Overlapping janitor runs can over-provision; trim idle extras.
		for i := 0; healthy > p.Size && i < len(ready); i++ {
			cp.Log.Info("pool larger than configured, removing a machine", "pool", name, "instance", ready[i])
			cp.TerminateInstances(ctx, ready[i])
			healthy--
		}
		need := p.Size - healthy
		if need > 5 {
			need = 5 // ramp up gradually
		}
		for i := 0; i < need; i++ {
			if err := cp.launchPoolInstance(ctx, name, spec, hash); err != nil {
				cp.Log.Error("pool launch failed", "pool", name, "err", err)
				if firstErr == nil {
					firstErr = err
				}
				break
			}
		}
	}
	return firstErr
}

func (cp *ControlPlane) launchPoolInstance(ctx context.Context, pool string, spec *config.JobSpec, hash string) error {
	img, err := cp.ResolveImage(ctx, spec.Image, spec.ImageSpec, spec.Arch, true)
	if err != nil {
		return err
	}
	rel, err := cp.RunnerRelease(ctx)
	if err != nil {
		return err
	}
	poolID := fmt.Sprintf("%s-%d-%s", pool, time.Now().Unix(), randHex(3))
	ac := &agentcfg.Config{
		Version: cp.Env.Version, Mode: agentcfg.ModePool, Platform: spec.Platform(), Arch: spec.Arch,
		Stack: cp.Env.Stack, Region: cp.Env.Region, Bucket: cp.Env.Bucket,
		RunnerVersion: rel.Version, RunnerURLs: cp.runnerURLs(ctx, rel, spec.Platform(), spec.Arch),
		Profile: spec.ImageSpec.Profile, ImageName: spec.Image, ImageOS: spec.ImageSpec.OS, User: spec.ImageSpec.User,
		UseNVMe: true, MarkerKey: "pools/" + poolID + ".json", LogKey: "logs/pools/" + poolID + ".log", Pool: pool,
	}
	if img.Baked {
		ac.Profile = "baked:" + spec.ImageSpec.Profile
	}
	ud, err := cp.buildUserData(ctx, ac)
	if err != nil {
		return err
	}
	tags := map[string]string{
		"Name": "truner pool " + pool, TagManaged: "true", TagStack: cp.Env.Stack, TagRole: "pool",
		TagPool: pool, TagPoolID: poolID, TagPoolState: poolPreparing, TagPoolHash: hash, TagImage: spec.Image, TagVersion: cp.Env.Version,
	}
	r, err := cp.Launch(ctx, LaunchInput{
		Name: "pool-" + poolID, Spec: spec, Image: img, UserData: ud, Subnets: cp.subnets(spec.Private),
		Tags: tags, ShutdownBehavior: ec2types.ShutdownBehaviorStop, ForceOnDemand: true,
	})
	if err != nil {
		return err
	}
	cp.Log.Info("pool machine launched", "pool", pool, "instance", r.InstanceID, "type", r.InstanceType)
	return nil
}
