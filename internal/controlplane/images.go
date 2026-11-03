package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/codician-team/truner/internal/agentcfg"
	"github.com/codician-team/truner/internal/config"
)

// ResolvedImage is a concrete AMI for a job.
type ResolvedImage struct {
	ID          string
	Name        string // truner image name
	RootDevice  string
	RootSizeGiB int
	Platform    string
	Arch        string
	Baked       bool
	CreatedAt   time.Time
}

type cachedImage struct {
	img *ResolvedImage
	at  time.Time
}

// ImageBakeable reports whether truner should maintain a baked AMI for img.
func ImageBakeable(img config.ImageSpec) bool {
	return config.BoolOr(img.Bake, false) && img.Profile != "" && img.Profile != "none"
}

// ResolveImage finds the AMI for a spec: the newest baked AMI when one
// exists, otherwise the base image (the agent then installs the profile at boot).
func (cp *ControlPlane) ResolveImage(ctx context.Context, name string, img config.ImageSpec, arch string, allowBaked bool) (*ResolvedImage, error) {
	key := fmt.Sprintf("%s|%s|%v", name, arch, allowBaked)
	cp.mu.Lock()
	if c, ok := cp.images[key]; ok && time.Since(c.at) < 3*time.Minute {
		cp.mu.Unlock()
		return c.img, nil
	}
	cp.mu.Unlock()
	var res *ResolvedImage
	var err error
	if allowBaked && ImageBakeable(img) {
		res, err = cp.latestBaked(ctx, name, arch)
		if err != nil {
			return nil, err
		}
	}
	if res == nil {
		res, err = cp.baseImage(ctx, name, img, arch)
		if err != nil {
			return nil, err
		}
	}
	cp.mu.Lock()
	cp.images[key] = cachedImage{img: res, at: time.Now()}
	cp.mu.Unlock()
	return res, nil
}

func (cp *ControlPlane) latestBaked(ctx context.Context, name, arch string) (*ResolvedImage, error) {
	imgs, err := cp.bakedImages(ctx, name, arch)
	if err != nil || len(imgs) == 0 {
		return nil, err
	}
	r := describeToResolved(imgs[0], name)
	r.Baked = true
	return r, nil
}

// bakedImages returns this stack's baked AMIs for an image, newest first.
func (cp *ControlPlane) bakedImages(ctx context.Context, name, arch string) ([]ec2types.Image, error) {
	out, err := cp.EC2.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{"self"},
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:" + TagStack), Values: []string{cp.Env.Stack}},
			{Name: aws.String("tag:" + TagImage), Values: []string{name}},
			{Name: aws.String("tag:" + TagArch), Values: []string{arch}},
			{Name: aws.String("state"), Values: []string{"available"}},
		},
	})
	if err != nil {
		return nil, err
	}
	imgs := out.Images
	sort.Slice(imgs, func(i, j int) bool { return aws.ToString(imgs[i].CreationDate) > aws.ToString(imgs[j].CreationDate) })
	return imgs, nil
}

func (cp *ControlPlane) baseImage(ctx context.Context, name string, img config.ImageSpec, arch string) (*ResolvedImage, error) {
	in := &ec2.DescribeImagesInput{}
	switch {
	case img.AMI != "":
		in.ImageIds = []string{img.AMI}
	case img.Name != "":
		owner := img.Owner
		if owner == "" {
			owner = "self"
		}
		in.Owners = []string{owner}
		in.Filters = []ec2types.Filter{
			{Name: aws.String("name"), Values: []string{img.Name}},
			{Name: aws.String("state"), Values: []string{"available"}},
			{Name: aws.String("architecture"), Values: []string{ec2Arch(arch)}},
		}
	case img.SSMParameter != "":
		p := strings.NewReplacer("{arch}", debArch(arch), "{march}", ec2Arch(arch)).Replace(img.SSMParameter)
		out, err := cp.SSM.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(p)})
		if err != nil {
			return nil, fmt.Errorf("image %s: resolve %s: %w", name, p, err)
		}
		in.ImageIds = []string{aws.ToString(out.Parameter.Value)}
	default:
		return nil, fmt.Errorf("image %s has no source", name)
	}
	out, err := cp.EC2.DescribeImages(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", name, err)
	}
	if len(out.Images) == 0 {
		return nil, fmt.Errorf("image %s: no matching AMI found", name)
	}
	imgs := out.Images
	sort.Slice(imgs, func(i, j int) bool { return aws.ToString(imgs[i].CreationDate) > aws.ToString(imgs[j].CreationDate) })
	r := describeToResolved(imgs[0], name)
	if r.Arch != arch {
		return nil, fmt.Errorf("image %s (%s) is %s, but %s was requested", name, r.ID, r.Arch, arch)
	}
	if img.Platform == config.PlatformWindows {
		r.Platform = config.PlatformWindows
	}
	return r, nil
}

func describeToResolved(i ec2types.Image, name string) *ResolvedImage {
	r := &ResolvedImage{ID: aws.ToString(i.ImageId), Name: name, RootDevice: aws.ToString(i.RootDeviceName), Platform: config.PlatformLinux, Arch: config.ArchX64}
	if i.Architecture == ec2types.ArchitectureValuesArm64 {
		r.Arch = config.ArchARM64
	}
	if i.Platform == ec2types.PlatformValuesWindows || strings.Contains(strings.ToLower(aws.ToString(i.PlatformDetails)), "windows") {
		r.Platform = config.PlatformWindows
	}
	for _, bd := range i.BlockDeviceMappings {
		if aws.ToString(bd.DeviceName) == r.RootDevice && bd.Ebs != nil {
			r.RootSizeGiB = int(aws.ToInt32(bd.Ebs.VolumeSize))
		}
	}
	if t, err := time.Parse(time.RFC3339, aws.ToString(i.CreationDate)); err == nil {
		r.CreatedAt = t
	}
	if r.RootDevice == "" {
		r.RootDevice = "/dev/sda1"
	}
	return r
}

func ec2Arch(a string) string {
	if a == config.ArchARM64 {
		return "arm64"
	}
	return "x86_64"
}

func debArch(a string) string {
	if a == config.ArchARM64 {
		return "arm64"
	}
	return "amd64"
}

// ---- baking ----

func bakePK(name, arch string) string { return "bake#" + name + "#" + arch }

// EnsureBake starts building an AMI for an image unless a build is already
// in progress. It returns the bake id (empty when nothing was started).
func (cp *ControlPlane) EnsureBake(ctx context.Context, name string, img config.ImageSpec, arch string, force bool) (string, error) {
	if !ImageBakeable(img) && !force {
		return "", nil
	}
	if img.Profile == "" || img.Profile == "none" {
		return "", fmt.Errorf("image %s has no profile to bake", name)
	}
	pk := bakePK(name, arch)
	cur, err := cp.Store.GetItem(ctx, pk)
	if err != nil {
		return "", err
	}
	if cur != nil {
		st := cur.Str("status")
		age := time.Since(time.Unix(cur.Int("ts"), 0))
		if (st == "building" || st == "imaging") && age < 3*time.Hour {
			return "", nil
		}
		// Back off after a failure so a broken image does not loop forever.
		if st == "failed" && age < 6*time.Hour && !force {
			return "", nil
		}
	}
	bakeID := fmt.Sprintf("%s-%s-%d", name, arch, time.Now().Unix())
	now := time.Now().Unix()
	if err := cp.Store.UpdateItem(ctx, pk, Item{"coll": collBakes, "ts": now, "status": "building", "bakeId": bakeID, "image": name, "arch": arch, "profile": img.Profile, "error": ""}); err != nil {
		return "", err
	}
	base, err := cp.baseImage(ctx, name, img, arch)
	if err != nil {
		cp.Store.UpdateItem(ctx, pk, Item{"status": "failed", "error": err.Error()})
		return "", err
	}
	rel, _ := cp.RunnerRelease(ctx)
	ac := &agentcfg.Config{
		Version:   cp.Env.Version,
		Mode:      agentcfg.ModeBake,
		Platform:  base.Platform,
		Arch:      arch,
		Stack:     cp.Env.Stack,
		Region:    cp.Env.Region,
		Bucket:    cp.Env.Bucket,
		Profile:   img.Profile,
		ImageName: name,
		ImageOS:   img.OS,
		User:      img.User,
		MarkerKey: "bakes/" + bakeID + ".json",
		LogKey:    "logs/bakes/" + bakeID + ".log",
	}
	if rel != nil {
		ac.RunnerVersion = rel.Version
		ac.RunnerURLs = cp.runnerURLs(ctx, rel, base.Platform, arch)
	}
	ud, err := cp.buildUserData(ctx, ac)
	if err != nil {
		cp.Store.UpdateItem(ctx, pk, Item{"status": "failed", "error": err.Error()})
		return "", err
	}
	disk := 16
	switch {
	case base.Platform == config.PlatformWindows:
		disk = 60
	case img.Profile == "full":
		disk = 40
	}
	if base.RootSizeGiB > disk {
		disk = base.RootSizeGiB
	}
	spec := &config.JobSpec{
		CPU:       config.IntRange{Min: 2, Max: 4},
		RAM:       config.IntRange{Min: 4},
		Arch:      arch,
		Spot:      config.SpotOff,
		Disk:      disk,
		ImageSpec: img,
	}
	tags := map[string]string{
		TagManaged: "true", TagStack: cp.Env.Stack, TagRole: "builder", TagImage: name, TagArch: arch,
		TagBakeID: bakeID, TagVersion: cp.Env.Version, "Name": "truner-bake-" + name + "-" + arch,
		TagDeadline: fmt.Sprint(time.Now().Add(3 * time.Hour).Unix()),
	}
	res, err := cp.Launch(ctx, LaunchInput{
		Name: "bake-" + bakeID, Spec: spec, Image: base, UserData: ud, Subnets: cp.subnets(false),
		Tags: tags, ShutdownBehavior: ec2types.ShutdownBehaviorStop, ForceOnDemand: true,
	})
	if err != nil {
		cp.Store.UpdateItem(ctx, pk, Item{"status": "failed", "error": err.Error()})
		return "", err
	}
	cp.Store.UpdateItem(ctx, pk, Item{"builder": res.InstanceID})
	cp.Log.Info("bake started", "image", name, "arch", arch, "bakeId", bakeID, "instance", res.InstanceID)
	return bakeID, nil
}

type bakeMarker struct {
	OK            bool   `json:"ok"`
	Error         string `json:"error,omitempty"`
	RunnerVersion string `json:"runnerVersion,omitempty"`
}

func (cp *ControlPlane) readMarker(ctx context.Context, key string, v any) (bool, error) {
	out, err := cp.S3.GetObject(ctx, &s3.GetObjectInput{Bucket: &cp.Env.Bucket, Key: &key})
	if err != nil {
		if strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), "StatusCode: 404") {
			return false, nil
		}
		return false, err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(io.LimitReader(out.Body, 1<<20))
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(data, v)
}

// ProcessBakes advances in-flight AMI builds.
func (cp *ControlPlane) ProcessBakes(ctx context.Context) error {
	items, err := cp.Store.ItemsInColl(ctx, collBakes)
	if err != nil {
		return err
	}
	var errs []error
	for _, it := range items {
		if err := cp.processBake(ctx, it); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (cp *ControlPlane) processBake(ctx context.Context, it Item) error {
	pk, status, builder := it.Str("pk"), it.Str("status"), it.Str("builder")
	started := time.Unix(it.Int("ts"), 0)
	fail := func(msg string) error {
		cp.Log.Error("bake failed", "image", it.Str("image"), "arch", it.Str("arch"), "err", msg)
		if builder != "" {
			cp.TerminateInstances(ctx, builder)
		}
		cp.alert(ctx, "truner: image bake failed", fmt.Sprintf("Building image %s (%s) failed: %s\nLogs: s3://%s/logs/bakes/%s.log", it.Str("image"), it.Str("arch"), msg, cp.Env.Bucket, it.Str("bakeId")))
		return cp.Store.UpdateItem(ctx, pk, Item{"status": "failed", "error": msg})
	}
	switch status {
	case "building":
		if builder == "" {
			if time.Since(started) > 15*time.Minute {
				return fail("builder never started")
			}
			return nil
		}
		out, err := cp.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{builder}})
		if err != nil {
			if strings.Contains(err.Error(), "NotFound") {
				return fail("builder instance disappeared")
			}
			return err
		}
		if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
			return fail("builder instance disappeared")
		}
		inst := out.Reservations[0].Instances[0]
		switch inst.State.Name {
		case ec2types.InstanceStateNameStopped:
			var m bakeMarker
			ok, err := cp.readMarker(ctx, "bakes/"+it.Str("bakeId")+".json", &m)
			if err != nil {
				return err
			}
			if !ok || !m.OK {
				msg := "builder stopped without reporting success"
				if m.Error != "" {
					msg = m.Error
				}
				return fail(msg)
			}
			ts := time.Now().UTC().Format("20060102-150405")
			tags := []ec2types.Tag{
				{Key: aws.String(TagManaged), Value: aws.String("true")},
				{Key: aws.String(TagStack), Value: aws.String(cp.Env.Stack)},
				{Key: aws.String(TagImage), Value: aws.String(it.Str("image"))},
				{Key: aws.String(TagArch), Value: aws.String(it.Str("arch"))},
				{Key: aws.String(TagVersion), Value: aws.String(cp.Env.Version)},
				{Key: aws.String("truner:runner-version"), Value: aws.String(m.RunnerVersion)},
				{Key: aws.String("Name"), Value: aws.String("truner-" + it.Str("image") + "-" + it.Str("arch"))},
			}
			img, err := cp.EC2.CreateImage(ctx, &ec2.CreateImageInput{
				InstanceId:  aws.String(builder),
				Name:        aws.String(truncate(fmt.Sprintf("truner-%s-%s-%s-%s", cp.Env.Stack, it.Str("image"), it.Str("arch"), ts), 128)),
				Description: aws.String("truner runner image " + it.Str("image") + " (" + it.Str("profile") + ")"),
				TagSpecifications: []ec2types.TagSpecification{
					{ResourceType: ec2types.ResourceTypeImage, Tags: tags},
					{ResourceType: ec2types.ResourceTypeSnapshot, Tags: tags},
				},
			})
			if err != nil {
				return fail("create image: " + err.Error())
			}
			cp.Log.Info("bake imaging", "image", it.Str("image"), "ami", aws.ToString(img.ImageId))
			return cp.Store.UpdateItem(ctx, pk, Item{"status": "imaging", "imageId": aws.ToString(img.ImageId), "imagingAt": time.Now().Unix()})
		case ec2types.InstanceStateNameTerminated, ec2types.InstanceStateNameShuttingDown:
			return fail("builder instance terminated")
		default:
			if time.Since(started) > 2*time.Hour {
				return fail("timed out after 2h")
			}
		}
	case "imaging":
		id := it.Str("imageId")
		out, err := cp.EC2.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{id}})
		if err != nil {
			return err
		}
		if len(out.Images) == 0 {
			return fail("image " + id + " disappeared")
		}
		switch out.Images[0].State {
		case ec2types.ImageStateAvailable:
			cp.TerminateInstances(ctx, builder)
			cp.Log.Info("bake done", "image", it.Str("image"), "arch", it.Str("arch"), "ami", id)
			cp.emit("ImagesBaked", 1, UnitCount)
			cp.mu.Lock()
			cp.images = map[string]cachedImage{}
			cp.mu.Unlock()
			if err := cp.pruneImages(ctx, it.Str("image"), it.Str("arch"), 2); err != nil {
				cp.Log.Warn("prune images failed", "err", err)
			}
			return cp.Store.UpdateItem(ctx, pk, Item{"status": "done", "doneAt": time.Now().Unix()})
		case ec2types.ImageStateFailed, ec2types.ImageStateError, ec2types.ImageStateInvalid:
			return fail("image creation failed: " + aws.ToString(out.Images[0].StateReason.Message))
		default:
			if time.Since(time.Unix(it.Int("imagingAt"), 0)) > 2*time.Hour {
				return fail("image creation timed out")
			}
		}
	}
	return nil
}

// pruneImages deregisters all but the newest `keep` baked AMIs (and their snapshots).
func (cp *ControlPlane) pruneImages(ctx context.Context, name, arch string, keep int) error {
	imgs, err := cp.bakedImages(ctx, name, arch)
	if err != nil {
		return err
	}
	for i, img := range imgs {
		if i < keep {
			continue
		}
		if err := cp.DeleteImage(ctx, img); err != nil {
			return err
		}
	}
	return nil
}

// DeleteImage deregisters an AMI and deletes its snapshots.
func (cp *ControlPlane) DeleteImage(ctx context.Context, img ec2types.Image) error {
	if _, err := cp.EC2.DeregisterImage(ctx, &ec2.DeregisterImageInput{ImageId: img.ImageId}); err != nil {
		return err
	}
	for _, bd := range img.BlockDeviceMappings {
		if bd.Ebs != nil && bd.Ebs.SnapshotId != nil {
			if _, err := cp.EC2.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: bd.Ebs.SnapshotId}); err != nil {
				cp.Log.Warn("delete snapshot failed", "snapshot", aws.ToString(bd.Ebs.SnapshotId), "err", err)
			}
		}
	}
	cp.Log.Info("deleted old image", "ami", aws.ToString(img.ImageId))
	return nil
}

// recordImageUse remembers that an image is in use so it keeps getting rebaked.
func (cp *ControlPlane) recordImageUse(ctx context.Context, name string, img config.ImageSpec, arch string) {
	if !ImageBakeable(img) {
		return
	}
	spec, _ := json.Marshal(img)
	cp.Store.PutItem(ctx, "imageuse#"+name+"#"+arch, Item{"coll": collImageUse, "ts": time.Now().Unix(), "image": name, "arch": arch, "spec": string(spec)}, 30*24*time.Hour, false)
}

// AutoBake rebuilds used images whose baked AMI is missing or stale.
func (cp *ControlPlane) AutoBake(ctx context.Context) error {
	g := cp.GlobalConfig(ctx)
	if !config.BoolOr(g.AutoBake, true) {
		return nil
	}
	interval := time.Duration(g.BakeIntervalDays) * 24 * time.Hour
	if interval == 0 {
		interval = 7 * 24 * time.Hour
	}
	items, err := cp.Store.ItemsInColl(ctx, collImageUse)
	if err != nil {
		return err
	}
	for _, it := range items {
		if time.Since(time.Unix(it.Int("ts"), 0)) > 14*24*time.Hour {
			continue
		}
		var img config.ImageSpec
		if json.Unmarshal([]byte(it.Str("spec")), &img) != nil {
			continue
		}
		name, arch := it.Str("image"), it.Str("arch")
		latest, err := cp.latestBaked(ctx, name, arch)
		if err != nil {
			cp.Log.Warn("autobake: list images failed", "err", err)
			continue
		}
		if latest != nil && time.Since(latest.CreatedAt) < interval {
			continue
		}
		if _, err := cp.EnsureBake(ctx, name, img, arch, false); err != nil {
			cp.Log.Warn("autobake failed", "image", name, "arch", arch, "err", err)
		}
	}
	return nil
}
