package cli

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/codician-team/truner/internal/controlplane"
	"github.com/codician-team/truner/internal/costs"
)

func TestSettledDays(t *testing.T) {
	from := time.Date(2026, 11, 3, 15, 0, 0, 0, time.UTC)
	// As measured on 2026-11-07 07:50 UTC: Nov 5 was complete, Nov 6 was not.
	got := settledDays(from, time.Date(2026, 11, 7, 7, 50, 0, 0, time.UTC))
	if strings.Join(got, " ") != "2026-11-03 2026-11-04 2026-11-05" {
		t.Fatalf("settled %v", got)
	}
	if got := settledDays(from, time.Date(2026, 11, 4, 23, 59, 0, 0, time.UTC)); len(got) != 0 {
		t.Fatalf("too early: %v", got)
	}
}

func TestStaleDays(t *testing.T) {
	day := func(s string) int64 {
		d, _ := time.Parse(time.DateTime, s)
		return d.Unix()
	}
	days := []string{"2026-11-03", "2026-11-04", "2026-11-05"}
	fetched := map[string]int64{
		"2026-11-03": day("2026-11-07 00:00:00"), // three days after its end: final
		"2026-11-04": day("2026-11-07 00:00:00"), // two days after its end: read again
	}
	if got := strings.Join(staleDays(days, fetched, 0), " "); got != "2026-11-04 2026-11-05" {
		t.Fatalf("stale %s", got)
	}
	// Tags activated later: the final day has to be read again, with its tags.
	if got := staleDays(days[:1], fetched, day("2026-11-09 00:00:00")); len(got) != 1 {
		t.Fatalf("after activation %v", got)
	}
}

func TestBackfillFrom(t *testing.T) {
	got := backfillFrom(time.Date(2026, 11, 7, 8, 45, 0, 0, time.UTC)).Format(time.RFC3339)
	if got != "2026-10-01T00:00:00Z" {
		t.Fatalf("from %s", got)
	}
	// The pattern and length AWS enforces on BackfillFrom.
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(([+-]\d\d:\d\d)|Z)$`).MatchString(got) || len(got) < 20 {
		t.Fatalf("AWS refuses %s", got)
	}
	if got := backfillFrom(time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)); got != time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("over the new year: %v", got)
	}
	// UTC decides the month, not the machine's time zone.
	east := time.FixedZone("east", 3*3600)
	if got := backfillFrom(time.Date(2026, 11, 1, 1, 0, 0, 0, east)); got != time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("in UTC it is still October: %v", got)
	}
}

func TestForeignMicroImage(t *testing.T) {
	stacks := []string{"truner", "truner-codician"}
	if got := foreignMicroImage([]string{"truner-micro-4", "truner-codician-micro-1"}, stacks); got != "" {
		t.Fatalf("all truner's: %q", got)
	}
	if got := foreignMicroImage([]string{"truner-micro-4", "canary-sandbox"}, stacks); got != "canary-sandbox" {
		t.Fatalf("foreign: %q", got)
	}
}

func TestBillError(t *testing.T) {
	for msg, want := range map[string]string{
		"AccessDeniedException: User is not authorized": "ce:GetCostAndUsage",
		"User not enabled for cost explorer access":     "",
		"DataUnavailableException":                      "no cost data",
		"boom":                                          "boom",
	} {
		got := billError(errors.New(msg)).Error()
		if !strings.Contains(got, want) || !strings.HasPrefix(got, "costs stay estimates") {
			t.Errorf("%s: %s", msg, got)
		}
	}
}

func TestBillBook(t *testing.T) {
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC).Unix()
	billed := controlplane.Job{InstanceType: "microvm-4vcpu", Lifecycle: "microvm", LaunchedAt: at, CompletedAt: at + 60, Cost: 0.10}
	spot := controlplane.Job{InstanceType: "c7a.xlarge", Lifecycle: "spot", LaunchedAt: at, CompletedAt: at + 60, Cost: 0.20}
	later := billed
	later.CompletedAt += 2 * 24 * 3600
	b := &billBook{stack: "truner", days: map[string]*costs.DayBill{
		"2026-11-05": {Ratios: map[string]float64{costs.MicroVMKey: 0.8}, Estimated: 0.10, Billed: 0.08, Platform: 0.3},
		"2026-11-07": {Platform: 0.1},
	}}
	if c, ok := b.cost(&billed); !ok || c < 0.0799 || c > 0.0801 {
		t.Fatalf("billed job: %v %v", c, ok)
	}
	if c, ok := b.cost(&spot); ok || c != 0.20 {
		t.Fatalf("EC2 without a bill line: %v %v", c, ok)
	}
	if c, ok := b.cost(&later); ok || c != 0.10 {
		t.Fatalf("a day without a bill: %v %v", c, ok)
	}
	var none *billBook
	if c, ok := none.cost(&billed); ok || c != 0.10 {
		t.Fatalf("no book: %v %v", c, ok)
	}
	if est, usd, through := b.month("2026-11"); est != 0.10 || usd != 0.08 || through != "2026-11-05" {
		t.Fatalf("month %v %v %s", est, usd, through)
	}
	if usd, through := b.platform("2026-11-06"); usd != 0.1 || through != "2026-11-07" {
		t.Fatalf("platform %v %s", usd, through)
	}
}
