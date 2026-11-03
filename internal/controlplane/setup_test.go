package controlplane

import (
	"context"
	"encoding/json"
	"html"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/codician-team/truner/internal/github"
)

func (h *harness) get(path string, query map[string]string) events.APIGatewayV2HTTPResponse {
	h.t.Helper()
	req := events.APIGatewayV2HTTPRequest{RawPath: path, QueryStringParameters: query}
	req.RequestContext.DomainName = "abc123.execute-api.us-east-1.amazonaws.com"
	req.RequestContext.HTTP.Method = "GET"
	req.RequestContext.HTTP.Path = path
	return h.cp.handleHTTP(context.Background(), req)
}

func (h *harness) storedApp() github.AppCredentials {
	h.t.Helper()
	h.aws.mu.Lock()
	defer h.aws.mu.Unlock()
	var c github.AppCredentials
	if v, ok := h.aws.params["/truner/test/github-app"]; ok {
		json.Unmarshal([]byte(v), &c)
	}
	return c
}

func TestGitHubAppSetup(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.gh.appOwner = "acme"
	h.aws.mu.Lock()
	delete(h.aws.params, "/truner/test/github-app") // a fresh installation has no app yet
	h.aws.mu.Unlock()
	admin := func(req AdminRequest) AdminResponse { return h.cp.handleAdmin(ctx, req) }
	status := func(state string) *SetupStatus {
		r := admin(AdminRequest{Truner: "github-setup-status", State: state})
		if !r.OK || r.Setup == nil {
			t.Fatalf("status: %+v", r)
		}
		return r.Setup
	}
	state := strings.Repeat("ab", 24)
	other := strings.Repeat("cd", 24)

	if r := admin(AdminRequest{Truner: "github-setup", State: "short", AppName: "x"}); r.OK {
		t.Fatal("a short state must be rejected")
	}
	if r := admin(AdminRequest{Truner: "github-setup", State: state, Owner: "acme", AppName: "truner-acme"}); !r.OK {
		t.Fatal(r.Message)
	}
	if s := status(state); s.Status != SetupPending {
		t.Fatalf("status = %+v", s)
	}

	// Only the registered state opens the setup page.
	if r := h.get("/github/setup", map[string]string{"state": other}); r.StatusCode != 410 {
		t.Fatalf("unknown state: %d", r.StatusCode)
	}
	r := h.get("/github/setup", map[string]string{"state": state})
	if r.StatusCode != 200 || !strings.Contains(r.Body, `action="https://github.com/organizations/acme/settings/apps/new?state=`+state+`"`) {
		t.Fatalf("setup page: %d %s", r.StatusCode, r.Body)
	}
	raw := r.Body[strings.Index(r.Body, `name="manifest" value="`)+len(`name="manifest" value="`):]
	raw = raw[:strings.Index(raw, `"`)]
	var manifest struct {
		Name        string            `json:"name"`
		Hook        map[string]any    `json:"hook_attributes"`
		RedirectURL string            `json:"redirect_url"`
		SetupURL    string            `json:"setup_url"`
		Public      bool              `json:"public"`
		Permissions map[string]string `json:"default_permissions"`
		Events      []string          `json:"default_events"`
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(raw)), &manifest); err != nil {
		t.Fatal(err)
	}
	base := "https://abc123.execute-api.us-east-1.amazonaws.com"
	if manifest.Name != "truner-acme" || manifest.Hook["url"] != base+"/webhook" || manifest.RedirectURL != base+"/github/callback" ||
		manifest.SetupURL != base+"/github/installed" || manifest.Public || manifest.Permissions["administration"] != "write" ||
		len(manifest.Events) != 1 || manifest.Events[0] != "workflow_job" {
		t.Fatalf("manifest = %+v", manifest)
	}

	// A callback with the wrong state does nothing.
	if r := h.get("/github/callback", map[string]string{"state": other, "code": "c0"}); r.StatusCode != 409 || h.gh.conversions != 0 {
		t.Fatalf("wrong state: %d, %d conversions", r.StatusCode, h.gh.conversions)
	}

	// The real callback stores the credentials and continues to the installation page.
	r = h.get("/github/callback", map[string]string{"state": state, "code": "c1"})
	if r.StatusCode != 302 || r.Headers["Location"] != "https://github.com/apps/truner-acme/installations/new" {
		t.Fatalf("callback: %d %v %s", r.StatusCode, r.Headers, r.Body)
	}
	if c := h.storedApp(); c.AppID != 77 || c.WebhookSecret != "new-secret" || c.Owner != "acme" {
		t.Fatalf("stored credentials = %+v", c)
	}
	if gh, err := h.cp.GitHub(ctx); err != nil || gh.App().AppID != 77 {
		t.Fatalf("control plane does not use the new app: %v", err)
	}
	if s := status(state); s.Status != SetupCreated || s.AppSlug != "truner-acme" || s.AppOwner != "acme" {
		t.Fatalf("status = %+v", s)
	}

	// The state cannot be replayed; the setup page now leads to the installation.
	if r := h.get("/github/callback", map[string]string{"state": state, "code": "c2"}); r.StatusCode != 409 || h.gh.conversions != 1 {
		t.Fatalf("replay: %d, %d conversions", r.StatusCode, h.gh.conversions)
	}
	if r := h.get("/github/setup", map[string]string{"state": state}); r.StatusCode != 302 {
		t.Fatalf("setup after creation: %d", r.StatusCode)
	}
	if r := h.get("/github/installed", map[string]string{"installation_id": "1", "setup_action": "install"}); r.StatusCode != 200 {
		t.Fatalf("installed page: %d", r.StatusCode)
	}

	// Without replace, a second setup cannot overwrite the stored app.
	s2 := strings.Repeat("ef", 24)
	admin(AdminRequest{Truner: "github-setup", State: s2, Owner: "acme", AppName: "truner-acme"})
	r = h.get("/github/callback", map[string]string{"state": s2, "code": "c3"})
	if r.StatusCode != 500 || !strings.Contains(r.Body, "already configured") || status(s2).Status != SetupFailed {
		t.Fatalf("overwrite without replace: %d %s", r.StatusCode, r.Body)
	}

	// An app created for another owner is never stored.
	s3 := strings.Repeat("12", 24)
	admin(AdminRequest{Truner: "github-setup", State: s3, Owner: "acme", AppName: "truner-acme", Replace: true})
	h.gh.appOwner = "evil"
	r = h.get("/github/callback", map[string]string{"state": s3, "code": "c4"})
	if r.StatusCode != 500 || !strings.Contains(r.Body, "belongs to evil") || h.storedApp().Owner != "acme" {
		t.Fatalf("foreign owner: %d %s", r.StatusCode, r.Body)
	}

	// A newer setup replaces an older pending one, and pending links expire.
	s4, s5 := strings.Repeat("34", 24), strings.Repeat("56", 24)
	admin(AdminRequest{Truner: "github-setup", State: s4, AppName: "truner-x"})
	admin(AdminRequest{Truner: "github-setup", State: s5, AppName: "truner-x"})
	if r := h.get("/github/setup", map[string]string{"state": s4}); r.StatusCode != 410 || status(s4).Status != SetupExpired {
		t.Fatalf("superseded link: %d", r.StatusCode)
	}
	r = h.get("/github/setup", map[string]string{"state": s5})
	if !strings.Contains(r.Body, `action="https://github.com/settings/apps/new?state=`+s5+`"`) {
		t.Fatalf("personal account setup page: %s", r.Body)
	}
	h.cp.Store.UpdateItem(ctx, setupPK, Item{"expiresAt": time.Now().Add(-time.Minute).Unix()})
	if r := h.get("/github/setup", map[string]string{"state": s5}); r.StatusCode != 410 {
		t.Fatalf("expired link: %d", r.StatusCode)
	}
	if r := h.get("/github/callback", map[string]string{"state": s5, "code": "c5"}); r.StatusCode != 409 {
		t.Fatalf("expired callback: %d", r.StatusCode)
	}
}
