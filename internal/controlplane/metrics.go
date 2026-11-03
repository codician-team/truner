package controlplane

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Metric units.
const (
	UnitCount   = "Count"
	UnitSeconds = "Seconds"
	UnitMillis  = "Milliseconds"
	UnitNone    = "None"
)

// emit writes a CloudWatch Embedded Metric Format record to stdout, which
// Lambda ships to CloudWatch Logs where it becomes a metric for free.
func (cp *ControlPlane) emit(name string, value float64, unit string) {
	rec := map[string]any{
		"_aws": map[string]any{
			"Timestamp": time.Now().UnixMilli(),
			"CloudWatchMetrics": []map[string]any{{
				"Namespace":  "Truner",
				"Dimensions": [][]string{{"Stack"}},
				"Metrics":    []map[string]string{{"Name": name, "Unit": unit}},
			}},
		},
		"Stack": cp.Env.Stack,
		name:    value,
	}
	b, err := json.Marshal(rec)
	if err == nil {
		fmt.Fprintln(os.Stdout, string(b))
	}
}
