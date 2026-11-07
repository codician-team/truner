package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestPrintMachineLog(t *testing.T) {
	objects := map[string]string{}
	get := func(ctx context.Context, key string) (io.ReadCloser, error) {
		if s, ok := objects[key]; ok {
			return io.NopCloser(strings.NewReader(s)), nil
		}
		return nil, errors.New("NoSuchKey")
	}
	const key = "logs/2026-11-05/truner-127285209781-4efa5b.log"
	var out strings.Builder
	if err := printMachineLog(context.Background(), get, "data", key, &out); err == nil || !strings.Contains(err.Error(), "no machine log at s3://data/"+key) {
		t.Fatalf("no log: %v", err)
	}
	objects["logs/2026-11-05/truner-127285209781-4efa5b.console.log"] = "console\n"
	if err := printMachineLog(context.Background(), get, "data", key, &out); err != nil || out.String() != "console\n" {
		t.Fatalf("a machine terminated before its job shows its console output: %v %q", err, out.String())
	}
	objects[key] = "agent\n"
	out.Reset()
	if err := printMachineLog(context.Background(), get, "data", key, &out); err != nil || out.String() != "agent\nconsole\n" {
		t.Fatalf("expected the log, then the console output: %v %q", err, out.String())
	}
}
