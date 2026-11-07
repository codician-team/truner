package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
)

// janitorFailure sums up the failures of one janitor task.
type janitorFailure struct {
	Task  string
	Count int
	Last  time.Time
	Err   string // of the last failure
}

// janitorFailures reads the janitor's "janitor task failed" log lines since a
// time. A failing task does not stop the others, so the log is the only place
// that shows one.
func janitorFailures(ctx context.Context, c cloudwatchlogs.FilterLogEventsAPIClient, group string, since time.Time) ([]janitorFailure, error) {
	const msg = "janitor task failed"
	p := cloudwatchlogs.NewFilterLogEventsPaginator(c, &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName: aws.String(group), StartTime: aws.Int64(since.UnixMilli()), FilterPattern: aws.String(`"` + msg + `"`),
	})
	by := map[string]*janitorFailure{}
	for pages := 0; p.HasMorePages() && pages < 50; pages++ {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, ev := range page.Events {
			var line struct{ Msg, Task, Err string }
			if json.Unmarshal([]byte(aws.ToString(ev.Message)), &line) != nil || line.Msg != msg {
				continue
			}
			f := by[line.Task]
			if f == nil {
				f = &janitorFailure{Task: line.Task}
				by[line.Task] = f
			}
			f.Count++
			if t := time.UnixMilli(aws.ToInt64(ev.Timestamp)); !t.Before(f.Last) {
				f.Last, f.Err = t, line.Err
			}
		}
	}
	out := make([]janitorFailure, 0, len(by))
	for _, f := range by {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out, nil
}

// checkJanitor reports janitor tasks that failed in the last 24 hours: as a
// problem when the last failure is less than an hour old, else as a warning.
func checkJanitor(ctx context.Context, u *ui, e *env, problems *int) {
	// A day of a busy janitor's log takes CloudWatch about 15 seconds to scan.
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	fails, err := janitorFailures(ctx, cloudwatchlogs.NewFromConfig(e.cfg), "/truner/"+e.name+"/janitor", time.Now().Add(-24*time.Hour))
	if err != nil {
		u.warn("could not read the janitor's log: %v", err)
		return
	}
	reportJanitor(u, e.name, fails, time.Now(), problems)
}

func reportJanitor(u *ui, stack string, fails []janitorFailure, now time.Time, problems *int) {
	if len(fails) == 0 {
		u.ok("no janitor task failures in the last 24h")
		return
	}
	throttled := false
	for _, f := range fails {
		text := fmt.Sprintf("janitor task %q failed %d time(s) in the last 24h, last %s: %s", f.Task, f.Count, ago(f.Last), oneLine(f.Err, 300))
		if now.Sub(f.Last) < time.Hour {
			*problems++
			u.bad("%s", text)
		} else {
			u.warn("%s", text)
		}
		throttled = throttled || strings.Contains(f.Err, "ThrottlingException")
	}
	cmd := "truner logs --control-plane --since 24h"
	if stack != "truner" {
		cmd += " --stack " + stack
	}
	u.info("`%s \"janitor task failed\"` shows each failure", cmd)
	if throttled {
		u.info("AWS throttled the calls. Installations in one account share the limit; truner backs off by itself, so this should not last")
	}
}

// oneLine joins an error's lines and shortens it to n characters.
func oneLine(s string, n int) string { return shorten(strings.Join(strings.Fields(s), " "), n) }
