package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-lambda-go/events"

	"github.com/codician-team/truner/internal/config"
)

// AdminRequest is a direct Lambda invocation from the CLI.
type AdminRequest struct {
	Truner string `json:"truner"` // action
	Image  string `json:"image,omitempty"`
	Arch   string `json:"arch,omitempty"`
	Force  bool   `json:"force,omitempty"`

	// GitHub App setup (github-setup, github-setup-status)
	State   string `json:"state,omitempty"`
	Owner   string `json:"owner,omitempty"`
	AppName string `json:"appName,omitempty"`
	Replace bool   `json:"replace,omitempty"`
}

// AdminResponse is returned to the CLI.
type AdminResponse struct {
	OK      bool         `json:"ok"`
	Message string       `json:"message,omitempty"`
	Version string       `json:"version,omitempty"`
	Setup   *SetupStatus `json:"setup,omitempty"`
}

// Handle dispatches any Lambda event: API Gateway (webhooks), SQS (launch
// queue), EventBridge (schedules and EC2 events) and admin invocations.
func (cp *ControlPlane) Handle(ctx context.Context, raw json.RawMessage) (any, error) {
	var probe struct {
		RequestContext *struct {
			HTTP *struct{} `json:"http"`
		} `json:"requestContext"`
		Records []struct {
			EventSource string `json:"eventSource"`
		} `json:"Records"`
		DetailType string `json:"detail-type"`
		Truner     string `json:"truner"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("unrecognized event: %w", err)
	}
	switch {
	case probe.RequestContext != nil && probe.RequestContext.HTTP != nil:
		var req events.APIGatewayV2HTTPRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return cp.handleHTTP(ctx, req), nil
	case len(probe.Records) > 0 && probe.Records[0].EventSource == "aws:sqs":
		var ev events.SQSEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, err
		}
		resp := events.SQSEventResponse{}
		for _, r := range ev.Records {
			if err := cp.HandleQueueMessage(ctx, r.Body); err != nil {
				cp.Log.Error("queue message failed", "id", r.MessageId, "err", err)
				resp.BatchItemFailures = append(resp.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: r.MessageId})
			}
		}
		return resp, nil
	case probe.DetailType != "":
		var ev events.CloudWatchEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, err
		}
		return nil, cp.handleEventBridge(ctx, ev)
	case probe.Truner != "":
		var req AdminRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return cp.handleAdmin(ctx, req), nil
	}
	return nil, fmt.Errorf("unrecognized event")
}

func (cp *ControlPlane) handleHTTP(ctx context.Context, req events.APIGatewayV2HTTPRequest) events.APIGatewayV2HTTPResponse {
	path := req.RawPath
	if path == "" {
		path = req.RequestContext.HTTP.Path
	}
	if req.RequestContext.HTTP.Method == "GET" {
		switch {
		case path == "/" || path == "/health" || path == "/healthz":
			return events.APIGatewayV2HTTPResponse{StatusCode: 200, Body: "truner " + cp.Env.Version + " ok"}
		case strings.HasSuffix(path, "/github/setup"):
			return cp.setupPage(ctx, req, path)
		case strings.HasSuffix(path, "/github/callback"):
			return cp.setupCallback(ctx, req)
		case strings.HasSuffix(path, "/github/installed"):
			return installedPage(cp.Env.LabelPrefix)
		}
	}
	if req.RequestContext.HTTP.Method != "POST" || !strings.HasSuffix(path, "/webhook") {
		return events.APIGatewayV2HTTPResponse{StatusCode: 404, Body: "not found"}
	}
	body := []byte(req.Body)
	if req.IsBase64Encoded {
		b, err := base64.StdEncoding.DecodeString(req.Body)
		if err != nil {
			return events.APIGatewayV2HTTPResponse{StatusCode: 400, Body: "bad body"}
		}
		body = b
	}
	headers := map[string]string{}
	for k, v := range req.Headers {
		headers[strings.ToLower(k)] = v
	}
	r := cp.HandleWebhook(ctx, headers, body)
	return events.APIGatewayV2HTTPResponse{StatusCode: r.Status, Body: r.Body, Headers: map[string]string{"Content-Type": "text/plain"}}
}

func (cp *ControlPlane) handleEventBridge(ctx context.Context, ev events.CloudWatchEvent) error {
	var d struct {
		InstanceID string `json:"instance-id"`
		State      string `json:"state"`
	}
	json.Unmarshal(ev.Detail, &d)
	switch ev.DetailType {
	case "Scheduled Event":
		return cp.RunJanitor(ctx)
	case "EC2 Spot Instance Interruption Warning":
		return cp.HandleSpotInterruption(ctx, d.InstanceID)
	case "EC2 Instance State-change Notification":
		return cp.HandleInstanceStateChange(ctx, d.InstanceID, d.State)
	}
	cp.Log.Info("ignoring event", "type", ev.DetailType)
	return nil
}

func (cp *ControlPlane) handleAdmin(ctx context.Context, req AdminRequest) AdminResponse {
	fail := func(err error) AdminResponse {
		return AdminResponse{OK: false, Message: err.Error(), Version: cp.Env.Version}
	}
	switch req.Truner {
	case "ping":
		_, err := cp.GitHub(ctx)
		if err != nil {
			return fail(err)
		}
		return AdminResponse{OK: true, Message: "pong", Version: cp.Env.Version}
	case "janitor":
		if err := cp.RunJanitor(ctx); err != nil {
			return fail(err)
		}
		return AdminResponse{OK: true, Message: "janitor run complete", Version: cp.Env.Version}
	case "github-setup":
		if err := cp.StartAppSetup(ctx, req.State, req.Owner, req.AppName, req.Replace); err != nil {
			return fail(err)
		}
		return AdminResponse{OK: true, Version: cp.Env.Version}
	case "github-setup-status":
		st, err := cp.AppSetupStatus(ctx, req.State)
		if err != nil {
			return fail(err)
		}
		return AdminResponse{OK: true, Version: cp.Env.Version, Setup: st}
	case "mirror-runner":
		if err := cp.MirrorRunner(ctx, true); err != nil {
			return fail(err)
		}
		return AdminResponse{OK: true, Message: "runner packages mirrored", Version: cp.Env.Version}
	case "bake":
		g := cp.GlobalConfig(ctx)
		name, img, suffix, err := config.Resolver{Global: g}.LookupImage(req.Image)
		if err != nil {
			return fail(err)
		}
		arch := req.Arch
		if suffix != "" {
			arch = suffix
		}
		if arch == "" {
			arch = config.ArchX64
		}
		if arch, err = config.NormalizeArch(arch); err != nil {
			return fail(err)
		}
		id, err := cp.EnsureBake(ctx, name, img, arch, true)
		if err != nil {
			return fail(err)
		}
		if id == "" {
			return AdminResponse{OK: true, Message: "a build of " + name + " (" + arch + ") is already in progress", Version: cp.Env.Version}
		}
		cp.recordImageUse(ctx, name, img, arch)
		return AdminResponse{OK: true, Message: "started building " + name + " (" + arch + "): " + id, Version: cp.Env.Version}
	}
	return fail(fmt.Errorf("unknown action %q", req.Truner))
}
