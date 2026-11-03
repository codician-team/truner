// Command truner-lambda is the control plane Lambda function. The same
// binary serves webhooks (API Gateway), the launch queue (SQS) and the
// janitor (EventBridge).
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/truner/truner/internal/controlplane"
	"github.com/truner/truner/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version.Version)
		return
	}
	cp, err := controlplane.New(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "truner: init failed:", err)
		os.Exit(1)
	}
	if cp.Env.Version == "" {
		cp.Env.Version = version.Version
	}
	lambda.Start(cp.Handle)
}
