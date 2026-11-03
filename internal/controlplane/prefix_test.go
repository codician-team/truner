package controlplane

import (
	"context"
	"strings"
	"testing"
)

// A job written for another installation's prefix gets no machine here. It is
// recorded once for `truner doctor`, however often GitHub delivers it.
func TestIntegrationForeignPrefix(t *testing.T) {
	h := newHarness(t)
	labels := []string{"truner-codician/cpu=8/run=900"}
	r := h.webhook("queued", 7001, labels, "", "")
	if r.Status != 200 || !strings.Contains(r.Body, `"truner-codician"`) || !strings.Contains(r.Body, `"truner"`) {
		t.Fatalf("the response must name both prefixes: %+v", r)
	}
	h.webhook("queued", 7001, labels, "", "")
	h.webhook("in_progress", 7001, labels, "other-runner", "")
	if len(h.aws.messages) != 0 {
		t.Fatalf("no machine may be launched, got %d queue messages", len(h.aws.messages))
	}
	items, err := h.cp.Store.ItemsInColl(context.Background(), CollForeignPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Int("jobId") != 7001 || items[0].Str("label") != labels[0] ||
		items[0].Str("prefix") != "truner-codician" || items[0].Int("installationId") != 1 || items[0].Str("repo") != "app" {
		t.Fatalf("expected one record of the job: %v", items)
	}
	if r := h.webhook("queued", 7002, []string{"ubuntu-latest"}, "", ""); r.Body != "not a truner job" {
		t.Fatalf("unrelated jobs are ignored quietly: %+v", r)
	}
}
