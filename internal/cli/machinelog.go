package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/codician-team/truner/internal/controlplane"
)

// printMachineLog prints a machine's log and the console output the control
// plane kept when it terminated the machine before it took its job. get reads
// an object of the data bucket.
func printMachineLog(ctx context.Context, get func(ctx context.Context, key string) (io.ReadCloser, error), bucket, key string, w io.Writer) error {
	found := false
	var first error
	for _, k := range []string{key, controlplane.ConsoleLogKey(key)} {
		body, err := get(ctx, k)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		found = true
		_, err = io.Copy(w, body)
		body.Close()
		if err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("no machine log at s3://%s/%s yet (it is uploaded when the machine finishes): %w", bucket, key, first)
	}
	return nil
}
