package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"

	"github.com/codician-team/truner/internal/config"
	"github.com/codician-team/truner/internal/costs"
)

type cachedPrice struct {
	price float64
	at    time.Time
}

// List prices (USD) for the parts of a machine outside its instance price, used
// for per-job estimates.
const (
	hoursPerMonth     = 730
	gp3PerGiBMonth    = 0.08
	gp3PerIOPSMonth   = 0.005 // above the included 3000 IOPS
	gp3PerMiBpsMonth  = 0.04  // above the included 125 MiB/s
	publicIPv4PerHour = 0.005
)

// HourlyPrice returns the USD/hour price of an instance. Spot prices are
// looked up for the exact AZ, on-demand prices through the Pricing API.
func (cp *ControlPlane) HourlyPrice(ctx context.Context, instanceType, az, lifecycle, platform string) (float64, error) {
	key := fmt.Sprintf("%s|%s|%s|%s", instanceType, az, lifecycle, platform)
	ttl := 7 * 24 * time.Hour
	if lifecycle == "spot" {
		ttl = time.Hour
	}
	cp.mu.Lock()
	if c, ok := cp.priceCache[key]; ok && time.Since(c.at) < ttl {
		cp.mu.Unlock()
		return c.price, nil
	}
	cp.mu.Unlock()
	pk := "price#" + cp.Env.Region + "#" + key
	if it, err := cp.Store.GetItem(ctx, pk); err == nil && it != nil && time.Since(time.Unix(it.Int("at"), 0)) < ttl {
		p := it.Float("price")
		cp.mu.Lock()
		cp.priceCache[key] = cachedPrice{price: p, at: time.Now()}
		cp.mu.Unlock()
		return p, nil
	}
	var p float64
	var err error
	if lifecycle == "spot" {
		p, err = cp.spotPrice(ctx, instanceType, az, platform)
	} else {
		p, err = cp.onDemandPrice(ctx, instanceType, platform)
	}
	if err != nil {
		return 0, err
	}
	cp.mu.Lock()
	cp.priceCache[key] = cachedPrice{price: p, at: time.Now()}
	cp.mu.Unlock()
	cp.Store.PutItem(ctx, pk, Item{"price": p, "at": time.Now().Unix()}, ttl, false)
	return p, nil
}

func (cp *ControlPlane) spotPrice(ctx context.Context, instanceType, az, platform string) (float64, error) {
	desc := "Linux/UNIX"
	if platform == config.PlatformWindows {
		desc = "Windows"
	}
	in := &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:       []ec2types.InstanceType{ec2types.InstanceType(instanceType)},
		ProductDescriptions: []string{desc},
		StartTime:           aws.Time(time.Now()),
	}
	if az != "" {
		in.AvailabilityZone = aws.String(az)
	}
	out, err := cp.EC2.DescribeSpotPriceHistory(ctx, in)
	if err != nil {
		return 0, err
	}
	if len(out.SpotPriceHistory) == 0 {
		return 0, fmt.Errorf("no spot price for %s in %s", instanceType, az)
	}
	return strconv.ParseFloat(aws.ToString(out.SpotPriceHistory[0].SpotPrice), 64)
}

func (cp *ControlPlane) onDemandPrice(ctx context.Context, instanceType, platform string) (float64, error) {
	os := "Linux"
	if platform == config.PlatformWindows {
		os = "Windows"
	}
	f := func(k, v string) pricingtypes.Filter {
		return pricingtypes.Filter{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String(k), Value: aws.String(v)}
	}
	out, err := cp.Pricing.GetProducts(ctx, &pricing.GetProductsInput{
		ServiceCode: aws.String("AmazonEC2"),
		Filters: []pricingtypes.Filter{
			f("instanceType", instanceType),
			f("regionCode", cp.Env.Region),
			f("operatingSystem", os),
			f("tenancy", "Shared"),
			f("preInstalledSw", "NA"),
			f("capacitystatus", "Used"),
			f("licenseModel", "No License required"),
		},
		MaxResults: aws.Int32(10),
	})
	if err != nil {
		return 0, err
	}
	for _, raw := range out.PriceList {
		if p, ok := parseOnDemand(raw); ok {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no on-demand price for %s", instanceType)
}

func parseOnDemand(raw string) (float64, bool) {
	var doc struct {
		Terms struct {
			OnDemand map[string]struct {
				PriceDimensions map[string]struct {
					PricePerUnit map[string]string `json:"pricePerUnit"`
				} `json:"priceDimensions"`
			} `json:"OnDemand"`
		} `json:"terms"`
	}
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return 0, false
	}
	for _, t := range doc.Terms.OnDemand {
		for _, d := range t.PriceDimensions {
			if s, ok := d.PricePerUnit["USD"]; ok {
				if p, err := strconv.ParseFloat(s, 64); err == nil && p > 0 {
					return p, true
				}
			}
		}
	}
	return 0, false
}

// subnetAZ returns a subnet's availability zone, or "" if it cannot be read.
// EC2 Fleet names only the subnet of the machines it launches in a subnet.
func (cp *ControlPlane) subnetAZ(ctx context.Context, subnet string) string {
	cp.mu.Lock()
	az, ok := cp.subnetAZs[subnet]
	cp.mu.Unlock()
	if ok {
		return az
	}
	out, err := cp.EC2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: []string{subnet}})
	if err != nil || len(out.Subnets) == 0 {
		cp.Log.Warn("could not read the subnet's availability zone", "subnet", subnet, "err", err)
		return ""
	}
	az = aws.ToString(out.Subnets[0].AvailabilityZone)
	cp.mu.Lock()
	cp.subnetAZs[subnet] = az
	cp.mu.Unlock()
	return az
}

// jobCost estimates the cost of running a machine for d.
func jobCost(pricePerHour, overheadPerHour float64, d time.Duration) float64 {
	return (pricePerHour + overheadPerHour) * billedDuration(d).Hours()
}

// billedDuration applies EC2's 60 second minimum (Linux and Windows).
func billedDuration(d time.Duration) time.Duration {
	return max(d, time.Minute)
}

// overheadPerHour is what a machine costs besides its instance: its disk, with
// any provisioned IOPS and throughput, and its public IPv4 address.
func (j *Job) overheadPerHour() float64 {
	perMonth := float64(j.DiskGiB) * gp3PerGiBMonth
	if j.DiskIOPS > 3000 {
		perMonth += float64(j.DiskIOPS-3000) * gp3PerIOPSMonth
	}
	if j.DiskMiBps > 125 {
		perMonth += float64(j.DiskMiBps-125) * gp3PerMiBpsMonth
	}
	h := perMonth / hoursPerMonth
	if j.PublicIP {
		h += publicIPv4PerHour
	}
	return h
}

// EndedAt returns when the job's cost was last counted (unix seconds).
func (j *Job) EndedAt() int64 {
	switch {
	case j.CompletedAt > 0:
		return j.CompletedAt
	case j.MachineDone > 0:
		return j.MachineDone
	}
	return j.UpdatedAt
}

// BillEstimate splits a job's estimate into what the AWS bill's machine lines
// cover and the rest, for comparing the job with the bill.
func (j *Job) BillEstimate(stack string) costs.JobEstimate {
	e := costs.JobEstimate{
		Stack: stack,
		Day:   time.Unix(j.EndedAt(), 0).UTC().Format(time.DateOnly),
		Key:   costs.BillKey(firstNonEmpty(j.RanOnType, j.InstanceType), firstNonEmpty(j.RanOnLifecycle, j.Lifecycle)),
	}
	if e.Key != costs.MicroVMKey && j.LaunchedAt > 0 {
		d := time.Duration(j.EndedAt()-j.LaunchedAt) * time.Second
		e.Overhead = min(j.overheadPerHour()*billedDuration(d).Hours(), j.Cost)
	}
	e.Machine = j.Cost - e.Overhead
	return e
}
