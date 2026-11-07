package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ce "github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"

	"github.com/codician-team/truner/internal/controlplane"
	"github.com/codician-team/truner/internal/costs"
	"github.com/codician-team/truner/internal/deploy"
)

const (
	billDaysColl = "billdays"
	billMetaPK   = "billmeta"
	costTagStack = "truner:stack"

	billSettleAfter  = 24 * time.Hour // a day's lines are complete this long after it ended
	billFinalAfter   = 72 * time.Hour // and hardly change after this
	billRefreshEvery = 12 * time.Hour // every Cost Explorer request costs $0.01
	billActivation   = 24 * time.Hour // AWS takes this long to apply newly activated tags
	billKeep         = 400 * 24 * time.Hour
)

// costTags are the tags to activate as cost allocation tags. truner:stack is
// what `truner costs` needs, the others are for the user's own reports.
var costTags = []string{costTagStack, "truner:repository", "truner:workflow"}

// billBook is what the bill says about this stack, per UTC day (cached in the
// stack's table, so that reading it costs nothing).
type billBook struct {
	stack string
	days  map[string]*costs.DayBill
}

// cost returns a job's cost and whether it comes from the bill. A nil book
// holds only estimates.
func (b *billBook) cost(j *controlplane.Job) (float64, bool) {
	if b == nil {
		return j.Cost, false
	}
	e := j.BillEstimate(b.stack)
	if c, ok := b.days[e.Day].Cost(e); ok {
		return c, true
	}
	return j.Cost, false
}

// month sums the billed days of a month (2006-01): what the jobs the bill covers
// were estimated at, what they cost on the bill, and the last such day.
func (b *billBook) month(month string) (estimated, billed float64, through string) {
	if b == nil {
		return 0, 0, ""
	}
	for day, d := range b.days {
		if strings.HasPrefix(day, month) && d.Billed > 0 {
			estimated += d.Estimated
			billed += d.Billed
			through = max(through, day)
		}
	}
	return estimated, billed, through
}

// platform sums this stack's storage, images and control plane since a day.
func (b *billBook) platform(since string) (usd float64, through string) {
	if b == nil {
		return 0, ""
	}
	for day, d := range b.days {
		if day >= since && d.Platform > 0 {
			usd += d.Platform
			through = max(through, day)
		}
	}
	return usd, through
}

// loadBill reads the cached bill. A cache that cannot be read means estimates.
func (e *env) loadBill(ctx context.Context) *billBook {
	items, err := e.store().ItemsInColl(ctx, billDaysColl)
	if err != nil {
		return nil
	}
	b := &billBook{stack: e.name, days: map[string]*costs.DayBill{}}
	for _, it := range items {
		d := &costs.DayBill{}
		if json.Unmarshal([]byte(it.Str("bill")), d) == nil {
			b.days[it.Str("day")] = d
		}
	}
	return b
}

// settledDays lists the UTC days since from whose bill is complete.
func settledDays(from, now time.Time) []string {
	var out []string
	for d := from.UTC().Truncate(24 * time.Hour); !d.Add(24 * time.Hour).Add(billSettleAfter).After(now); d = d.Add(24 * time.Hour) {
		out = append(out, d.Format(time.DateOnly))
	}
	return out
}

// staleDays leaves out the days whose bill was read when it was final, and after
// the tags were activated. Those will not change, so they cost no more requests.
func staleDays(days []string, fetchedAt map[string]int64, refetchFrom int64) []string {
	var out []string
	for _, day := range days {
		t, err := time.Parse(time.DateOnly, day)
		if err != nil {
			continue
		}
		got := fetchedAt[day]
		if got < t.Add(24*time.Hour+billFinalAfter).Unix() || got < refetchFrom {
			out = append(out, day)
		}
	}
	return out
}

// jobEstimates returns the estimates of a store's jobs that ended on the days.
// Jobs are stored by the day they were queued, so that day's predecessor counts.
func jobEstimates(ctx context.Context, store *controlplane.Store, stack string, days []string) ([]costs.JobEstimate, error) {
	want := map[string]bool{}
	colls := map[string]bool{}
	for _, day := range days {
		t, err := time.Parse(time.DateOnly, day)
		if err != nil {
			continue
		}
		want[day] = true
		colls[controlplane.JobColl(t)] = true
		colls[controlplane.JobColl(t.Add(-24*time.Hour))] = true
	}
	var out []costs.JobEstimate
	for coll := range colls {
		jobs, err := store.JobsInColl(ctx, coll)
		if err != nil {
			return nil, err
		}
		for i := range jobs {
			if e := jobs[i].BillEstimate(stack); jobs[i].Cost > 0 && want[e.Day] {
				out = append(out, e)
			}
		}
	}
	return out, nil
}

// refreshBill reads the bill of the settled days since from that have jobs, unless
// that was tried in the last 12 hours or they are final, and caches what it says.
func (e *env) refreshBill(ctx context.Context, from, now time.Time, force bool) error {
	store := e.store()
	meta, err := store.GetItem(ctx, billMetaPK)
	if err != nil {
		return err
	}
	if !force && now.Sub(time.Unix(meta.Int("attemptAt"), 0)) < billRefreshEvery {
		return nil
	}
	items, err := store.ItemsInColl(ctx, billDaysColl)
	if err != nil {
		return err
	}
	fetchedAt := map[string]int64{}
	for _, it := range items {
		fetchedAt[it.Str("day")] = it.Int("fetchedAt")
	}
	days := staleDays(settledDays(from, now), fetchedAt, meta.Int("refetchFrom"))
	if len(days) == 0 {
		return nil
	}
	jobs, err := jobEstimates(ctx, store, e.name, days)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return nil
	}
	first, last := jobs[0].Day, jobs[0].Day
	for _, j := range jobs {
		first, last = min(first, j.Day), max(last, j.Day)
	}
	// Before the request, so that one that fails is not repeated on every command.
	if err := store.UpdateItem(ctx, billMetaPK, controlplane.Item{"attemptAt": now.Unix()}); err != nil {
		return err
	}
	lines, err := e.billLines(ctx, first, last)
	if err != nil {
		return err
	}
	jobs, microOK, microErr := e.withOtherStacks(ctx, jobs, lines, days)
	bills := costs.Reconcile(e.name, lines, jobs, microOK)
	tagged := int64(0)
	for _, l := range lines {
		if l.Stack == e.name {
			tagged = 1
		}
	}
	if err := store.UpdateItem(ctx, billMetaPK, controlplane.Item{"tagged": tagged}); err != nil {
		return err
	}
	withJobs := map[string]bool{}
	for _, j := range jobs {
		withJobs[j.Day] = true
	}
	for _, day := range days {
		// A day with jobs but nothing on the bill is saved too, so that it is
		// not requested again once it is final.
		if !withJobs[day] && bills[day] == nil {
			continue
		}
		bill := bills[day]
		if bill == nil {
			bill = &costs.DayBill{}
		}
		data, err := json.Marshal(bill)
		if err != nil {
			return err
		}
		t, _ := time.Parse(time.DateOnly, day)
		it := controlplane.Item{"coll": billDaysColl, "ts": t.Unix(), "day": day, "bill": string(data), "fetchedAt": now.Unix()}
		if _, err := store.PutItem(ctx, "billday#"+day, it, billKeep, false); err != nil {
			return err
		}
	}
	return microErr
}

// withOtherStacks adds the micro jobs of the region's other truner stacks, which
// share the MicroVM lines of the bill. The lines can be split only if nothing
// but truner runs MicroVMs in the region.
func (e *env) withOtherStacks(ctx context.Context, jobs []costs.JobEstimate, lines []costs.BillLine, days []string) ([]costs.JobEstimate, bool, error) {
	micro := false
	for _, l := range lines {
		micro = micro || costs.UsageKey(l.Usage) == costs.MicroVMKey
	}
	if !micro {
		return jobs, true, nil
	}
	stacks, err := deploy.ListStacks(ctx, e.cfg)
	if err != nil {
		return jobs, false, fmt.Errorf("MicroVM costs stay estimates: could not list the region's truner stacks: %w", err)
	}
	var images []string
	p := lambdamicrovms.NewListMicrovmImagesPaginator(lambdamicrovms.NewFromConfig(e.cfg), &lambdamicrovms.ListMicrovmImagesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return jobs, false, fmt.Errorf("MicroVM costs stay estimates: could not list MicroVM images: %w", err)
		}
		for _, it := range page.Items {
			images = append(images, aws.ToString(it.Name))
		}
	}
	if name := foreignMicroImage(images, stacks); name != "" {
		return jobs, false, fmt.Errorf("MicroVM costs stay estimates: MicroVM image %q is not truner's, and the bill cannot tell MicroVMs apart", name)
	}
	for _, name := range stacks {
		if name == e.name {
			continue
		}
		st, err := deploy.Describe(ctx, e.cfg, name)
		if err != nil {
			return jobs, false, fmt.Errorf("MicroVM costs stay estimates: could not read stack %s: %w", name, err)
		}
		other := &controlplane.Store{DB: e.ddb(), Table: st.Outputs["Table"]}
		more, err := jobEstimates(ctx, other, name, days)
		if err != nil {
			return jobs, false, fmt.Errorf("MicroVM costs stay estimates: could not read the jobs of stack %s: %w", name, err)
		}
		jobs = append(jobs, more...)
	}
	return jobs, true, nil
}

// foreignMicroImage returns the first MicroVM image no truner stack made.
func foreignMicroImage(images, stacks []string) string {
	for _, image := range images {
		ours := false
		for _, s := range stacks {
			ours = ours || strings.HasPrefix(image, s+"-micro-")
		}
		if !ours {
			return image
		}
	}
	return ""
}

func (e *env) costExplorer() *ce.Client {
	cfg := e.cfg.Copy()
	cfg.Region = "us-east-1" // Cost Explorer has one endpoint
	return ce.NewFromConfig(cfg)
}

// billLines reads what AWS billed in this region for the days first to last,
// grouped by the truner:stack tag, which is empty while the tag is not active.
func (e *env) billLines(ctx context.Context, first, last string) ([]costs.BillLine, error) {
	t, err := time.Parse(time.DateOnly, last)
	if err != nil {
		return nil, err
	}
	in := &ce.GetCostAndUsageInput{
		TimePeriod:  &cetypes.DateInterval{Start: aws.String(first), End: aws.String(t.Add(24 * time.Hour).Format(time.DateOnly))},
		Granularity: cetypes.GranularityDaily,
		Metrics:     []string{"UnblendedCost"},
		Filter: &cetypes.Expression{And: []cetypes.Expression{
			{Dimensions: &cetypes.DimensionValues{Key: cetypes.DimensionRegion, Values: []string{e.cfg.Region}}},
			{Dimensions: &cetypes.DimensionValues{Key: cetypes.DimensionRecordType, Values: []string{"Usage"}}}, // not credits, refunds or tax
		}},
		GroupBy: []cetypes.GroupDefinition{
			{Type: cetypes.GroupDefinitionTypeTag, Key: aws.String(costTagStack)},
			{Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String("USAGE_TYPE")},
		},
	}
	client := e.costExplorer()
	var out []costs.BillLine
	for {
		page, err := client.GetCostAndUsage(ctx, in)
		if err != nil {
			return nil, billError(err)
		}
		for _, r := range page.ResultsByTime {
			for _, g := range r.Groups {
				if len(g.Keys) != 2 {
					continue
				}
				usd, _ := strconv.ParseFloat(aws.ToString(g.Metrics["UnblendedCost"].Amount), 64)
				out = append(out, costs.BillLine{Day: aws.ToString(r.TimePeriod.Start), Stack: strings.TrimPrefix(g.Keys[0], costTagStack+"$"), Usage: g.Keys[1], Cost: usd})
			}
		}
		if page.NextPageToken == nil {
			return out, nil
		}
		in.NextPageToken = page.NextPageToken
	}
}

// billError says what to do about a request Cost Explorer refused.
func billError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not enabled for cost explorer"):
		return errors.New("costs stay estimates: Cost Explorer is off in this account. Open Cost Explorer once in the AWS Billing console; AWS then needs about a day to prepare the data")
	case strings.Contains(msg, "AccessDenied") || strings.Contains(msg, "not authorized"):
		return errors.New("costs stay estimates: allow ce:GetCostAndUsage for this AWS identity, or hide this with --estimates-only")
	case strings.Contains(msg, "DataUnavailable"):
		return errors.New("costs stay estimates: AWS has no cost data for these days yet")
	}
	return fmt.Errorf("costs stay estimates: Cost Explorer: %w", err)
}

// backfillFrom is the first of last month: AWS applies tags to earlier bills only
// from the first of a month.
func backfillFrom(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
}

// activateCostTags turns truner's tags into cost allocation tags, which is what
// lets the bill tell truner's machines from other EC2 in the account, and asks
// AWS to apply them to the bills since last month.
func (e *env) activateCostTags(ctx context.Context, u *ui, now time.Time) error {
	client := e.costExplorer()
	status := make([]cetypes.CostAllocationTagStatusEntry, 0, len(costTags))
	for _, t := range costTags {
		status = append(status, cetypes.CostAllocationTagStatusEntry{TagKey: aws.String(t), Status: cetypes.CostAllocationTagStatusActive})
	}
	out, err := client.UpdateCostAllocationTagsStatus(ctx, &ce.UpdateCostAllocationTagsStatusInput{CostAllocationTagsStatus: status})
	if err != nil {
		if strings.Contains(err.Error(), "AccessDenied") || strings.Contains(err.Error(), "not authorized") {
			return errors.New("not allowed to activate cost allocation tags: allow ce:UpdateCostAllocationTagsStatus for this AWS identity. In an AWS Organization, only the management account can do this")
		}
		return fmt.Errorf("activate cost allocation tags: %w", err)
	}
	failed := map[string]string{}
	for _, f := range out.Errors {
		failed[aws.ToString(f.TagKey)] = aws.ToString(f.Message)
	}
	for _, t := range costTags {
		if msg, bad := failed[t]; bad {
			u.warn("%s was not activated: %s", t, msg)
		} else {
			u.ok("%s is an active cost allocation tag", t)
		}
	}
	if msg, bad := failed[costTagStack]; bad {
		return fmt.Errorf("%s is what `truner costs` needs: %s. AWS lists a tag about a day after the first machine carried it; try again then", costTagStack, msg)
	}
	from := backfillFrom(now)
	_, err = client.StartCostAllocationTagBackfill(ctx, &ce.StartCostAllocationTagBackfillInput{BackfillFrom: aws.String(from.Format(time.RFC3339))})
	var limit *cetypes.BackfillLimitExceededException
	switch {
	case errors.As(err, &limit):
		u.ok("AWS is already applying them to earlier bills: it takes one such request every 24 hours")
	case err != nil:
		u.warn("AWS did not apply the tags to earlier bills, so EC2 stays estimated for days before today. Run `truner costs --activate-tags` again in a day: %v", err)
	default:
		u.ok("AWS applies them to the bills since %s", from.Format(time.DateOnly))
	}
	if err := e.store().UpdateItem(ctx, billMetaPK, controlplane.Item{"refetchFrom": now.Add(billActivation).Unix(), "attemptAt": int64(0)}); err != nil {
		return err
	}
	u.printf("AWS needs up to 24 hours. After that, `truner costs` shows what EC2 machines were billed.\n")
	return nil
}
