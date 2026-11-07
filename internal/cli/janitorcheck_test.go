package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

type fakeLogs struct {
	pages [][]cwtypes.FilteredLogEvent
	in    []*cloudwatchlogs.FilterLogEventsInput
}

func (f *fakeLogs) FilterLogEvents(ctx context.Context, in *cloudwatchlogs.FilterLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	f.in = append(f.in, in)
	out := &cloudwatchlogs.FilterLogEventsOutput{Events: f.pages[len(f.in)-1]}
	if len(f.in) < len(f.pages) {
		out.NextToken = aws.String("next")
	}
	return out, nil
}

func logEvent(t time.Time, msg string) cwtypes.FilteredLogEvent {
	return cwtypes.FilteredLogEvent{Timestamp: aws.Int64(t.UnixMilli()), Message: aws.String(msg)}
}

func TestJanitorFailures(t *testing.T) {
	now := time.Now()
	throttled := `{"time":"x","level":"ERROR","msg":"janitor task failed","task":"microvms","err":"operation error Lambda Microvms: ListMicrovms, exceeded maximum number of attempts, 3, ThrottlingException: "}`
	f := &fakeLogs{pages: [][]cwtypes.FilteredLogEvent{
		{
			logEvent(now.Add(-5*time.Hour), throttled),
			logEvent(now.Add(-3*time.Hour), `{"level":"ERROR","msg":"janitor task failed","task":"reruns","err":"github: 500\nsecond line"}`),
		},
		{
			logEvent(now.Add(-10*time.Minute), throttled),
			logEvent(now.Add(-time.Minute), `not json, mentions janitor task failed`),
			logEvent(now.Add(-time.Minute), `{"level":"INFO","msg":"janitor task done","task":"jobs"}`),
		},
	}}
	got, err := janitorFailures(context.Background(), f, "/truner/acme/janitor", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.in) != 2 || aws.ToString(f.in[0].LogGroupName) != "/truner/acme/janitor" || aws.ToString(f.in[0].FilterPattern) != `"janitor task failed"` {
		t.Fatalf("unexpected requests: %+v", f.in)
	}
	if len(got) != 2 || got[0].Task != "microvms" || got[0].Count != 2 || !strings.Contains(got[0].Err, "ThrottlingException") ||
		got[1].Task != "reruns" || got[1].Count != 1 {
		t.Fatalf("unexpected failures: %+v", got)
	}

	var out strings.Builder
	problems := 0
	reportJanitor(&ui{out: &out}, "truner-codician", got, now, &problems)
	s := out.String()
	if problems != 1 || !strings.Contains(s, "✗ janitor task \"microvms\" failed 2 time(s)") || !strings.Contains(s, "! janitor task \"reruns\"") ||
		!strings.Contains(s, "github: 500 second line") || !strings.Contains(s, "--stack truner-codician") || !strings.Contains(s, "throttled") {
		t.Fatalf("a failure in the last hour is a problem, an older one a warning:\n%s", s)
	}

	out.Reset()
	reportJanitor(&ui{out: &out}, "truner", nil, now, &problems)
	if problems != 1 || !strings.Contains(out.String(), "no janitor task failures") {
		t.Fatalf("no failures: %q", out.String())
	}
}
