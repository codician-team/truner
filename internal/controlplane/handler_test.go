package controlplane

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
)

func TestHandleDispatch(t *testing.T) {
	cp := NewWithConfig(&Env{Stack: "t", Region: "us-east-1", Version: "v9"}, aws.Config{Region: "us-east-1"})
	ctx := context.Background()

	health := `{"version":"2.0","rawPath":"/healthz","requestContext":{"http":{"method":"GET","path":"/healthz"}}}`
	out, err := cp.Handle(ctx, json.RawMessage(health))
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := out.(events.APIGatewayV2HTTPResponse); !ok || r.StatusCode != 200 || r.Body != "truner v9 ok" {
		t.Fatalf("health check: %#v", out)
	}
	notFound := `{"version":"2.0","rawPath":"/nope","requestContext":{"http":{"method":"POST","path":"/nope"}}}`
	out, _ = cp.Handle(ctx, json.RawMessage(notFound))
	if r := out.(events.APIGatewayV2HTTPResponse); r.StatusCode != 404 {
		t.Fatalf("unknown path: %d", r.StatusCode)
	}
	out, err = cp.Handle(ctx, json.RawMessage(`{"truner":"nope"}`))
	if r, ok := out.(AdminResponse); err != nil || !ok || r.OK || r.Version != "v9" {
		t.Fatalf("admin: %#v %v", out, err)
	}
	if _, err := cp.Handle(ctx, json.RawMessage(`{"detail-type":"Something Else","detail":{}}`)); err != nil {
		t.Fatalf("unknown EventBridge events are ignored: %v", err)
	}
	sqsEv := `{"Records":[{"messageId":"m1","eventSource":"aws:sqs","body":"not json"}]}`
	out, err = cp.Handle(ctx, json.RawMessage(sqsEv))
	if r, ok := out.(events.SQSEventResponse); err != nil || !ok || len(r.BatchItemFailures) != 0 {
		t.Fatalf("malformed queue messages are dropped, not retried: %#v %v", out, err)
	}
	if _, err := cp.Handle(ctx, json.RawMessage(`{"hello":1}`)); err == nil {
		t.Fatal("unknown events must error")
	}
}
