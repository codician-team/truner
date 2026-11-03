package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/codician-team/truner/internal/github"
	"github.com/codician-team/truner/internal/version"
)

// GitHub App setup runs through the control plane's own endpoint, so nothing
// has to keep running on the user's machine while they click through GitHub:
//
//  1. The CLI registers a pending setup with a random state (admin request).
//  2. The user opens <api>/github/setup?state=..., which posts the app
//     manifest to GitHub.
//  3. GitHub redirects to <api>/github/callback with a code. The control plane
//     exchanges it for the app's credentials, stores them in SSM and sends the
//     browser on to the app's installation page.
//  4. After the installation GitHub redirects to <api>/github/installed.
//
// The CLI polls the setup's status. This works the same from a laptop, an SSH
// session or an AI agent whose commands get interrupted.

// AppPermissions are what truner needs, and why:
//   - administration:write registers ephemeral runners on each repository
//   - actions:write reads jobs and re-runs jobs killed by spot interruptions
//   - contents:read reads .github/truner.yml
var AppPermissions = map[string]string{
	"administration": "write",
	"actions":        "write",
	"contents":       "read",
	"metadata":       "read",
}

// SetupTTL is how long a setup link stays valid.
const SetupTTL = time.Hour

const setupPK = "github-setup"

// Setup statuses.
const (
	SetupPending    = "pending"
	SetupConverting = "converting"
	SetupCreated    = "created"
	SetupFailed     = "failed"
	SetupExpired    = "expired"
)

// SetupStatus reports a GitHub App setup to the CLI.
type SetupStatus struct {
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	AppName  string `json:"appName,omitempty"`
	AppSlug  string `json:"appSlug,omitempty"`
	AppOwner string `json:"appOwner,omitempty"`
}

func hashState(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])
}

// StartAppSetup registers a pending setup. It replaces any earlier one, so
// only the newest link works. Only a hash of the state is stored.
func (cp *ControlPlane) StartAppSetup(ctx context.Context, state, owner, name string, replace bool) error {
	if len(state) < 32 {
		return errors.New("setup state must be at least 32 characters")
	}
	if name == "" {
		return errors.New("missing app name")
	}
	_, err := cp.Store.PutItem(ctx, setupPK, Item{
		"stateHash": hashState(state),
		"status":    SetupPending,
		"owner":     owner,
		"appName":   name,
		"replace":   replace,
		"expiresAt": time.Now().Add(SetupTTL).Unix(),
	}, SetupTTL+24*time.Hour, false)
	return err
}

// AppSetupStatus returns the state of the setup started with state.
func (cp *ControlPlane) AppSetupStatus(ctx context.Context, state string) (*SetupStatus, error) {
	it, err := cp.Store.GetItem(ctx, setupPK)
	if err != nil {
		return nil, err
	}
	if it == nil || it.Str("stateHash") != hashState(state) {
		return &SetupStatus{Status: SetupExpired, Message: "this setup was replaced by a newer one"}, nil
	}
	st := &SetupStatus{
		Status: it.Str("status"), Message: it.Str("message"),
		AppName: it.Str("appName"), AppSlug: it.Str("appSlug"), AppOwner: it.Str("appOwner"),
	}
	if st.Status == SetupPending && time.Now().Unix() > it.Int("expiresAt") {
		st.Status, st.Message = SetupExpired, "the setup link expired"
	}
	return st, nil
}

// claimSetup moves the setup matching state from pending to converting, so a
// state can be used exactly once.
func (cp *ControlPlane) claimSetup(ctx context.Context, state string) (Item, error) {
	cond := expression.Name("stateHash").Equal(expression.Value(hashState(state))).
		And(expression.Name("status").Equal(expression.Value(SetupPending))).
		And(expression.Name("expiresAt").GreaterThan(expression.Value(time.Now().Unix())))
	expr, err := expression.NewBuilder().
		WithUpdate(expression.Set(expression.Name("status"), expression.Value(SetupConverting))).
		WithCondition(cond).Build()
	if err != nil {
		return nil, err
	}
	_, err = cp.Store.DB.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &cp.Store.Table,
		Key:                       map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: setupPK}},
		UpdateExpression:          expr.Update(),
		ConditionExpression:       expr.Condition(),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
	})
	if isConditionFailed(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cp.Store.GetItem(ctx, setupPK)
}

// errAppExists means credentials are stored already and the setup may not replace them.
var errAppExists = errors.New("a GitHub App is already configured for this installation")

// StoreAppCredentials saves the GitHub App's credentials as a SecureString.
func StoreAppCredentials(ctx context.Context, client *ssm.Client, name string, creds *github.AppCredentials, overwrite bool) error {
	data, _ := json.Marshal(creds)
	tier := ssmtypes.ParameterTierStandard
	if len(data) > 4000 {
		tier = ssmtypes.ParameterTierAdvanced
	}
	_, err := client.PutParameter(ctx, &ssm.PutParameterInput{
		Name: &name, Value: aws.String(string(data)), Type: ssmtypes.ParameterTypeSecureString,
		Overwrite: aws.Bool(overwrite), Tier: tier, Description: aws.String("truner GitHub App credentials"),
	})
	var exists *ssmtypes.ParameterAlreadyExists
	if errors.As(err, &exists) {
		return errAppExists
	}
	if err != nil {
		return fmt.Errorf("store GitHub App credentials: %w", err)
	}
	return nil
}

// apiBase is the public base URL of the HTTP API, derived from a request to
// one of its paths.
func apiBase(req events.APIGatewayV2HTTPRequest, path, suffix string) string {
	return "https://" + req.RequestContext.DomainName + strings.TrimSuffix(path, suffix)
}

func (cp *ControlPlane) setupPage(ctx context.Context, req events.APIGatewayV2HTTPRequest, path string) events.APIGatewayV2HTTPResponse {
	state := req.QueryStringParameters["state"]
	st, err := cp.AppSetupStatus(ctx, state)
	if err != nil {
		cp.Log.Error("read setup", "err", err)
		return setupHTML(500, "Something went wrong", "<p>Please try again in a moment.</p>")
	}
	switch {
	case state == "" || st.Status == SetupExpired:
		return setupHTML(410, "This setup link has expired",
			"<p>Run <code>truner install</code> again to get a new link.</p>")
	case st.Status == SetupCreated:
		return redirect("https://github.com/apps/" + url.PathEscape(st.AppSlug) + "/installations/new")
	case st.Status != SetupPending:
		return setupHTML(409, "This setup link was already used",
			"<p>Run <code>truner install</code> again to get a new link.</p>")
	}
	it, _ := cp.Store.GetItem(ctx, setupPK)
	owner := it.Str("owner")
	base := apiBase(req, path, "/github/setup")
	manifest, _ := json.Marshal(map[string]any{
		"name":                it.Str("appName"),
		"url":                 "https://github.com/" + version.Repo,
		"description":         "Ephemeral GitHub Actions runners in our own AWS account, powered by truner.",
		"hook_attributes":     map[string]any{"url": base + "/webhook", "active": true},
		"redirect_url":        base + "/github/callback",
		"setup_url":           base + "/github/installed",
		"public":              false,
		"default_permissions": AppPermissions,
		"default_events":      []string{"workflow_job"},
	})
	target := "https://github.com/settings/apps/new?state=" + url.QueryEscape(state)
	if owner != "" {
		target = "https://github.com/organizations/" + url.PathEscape(owner) + "/settings/apps/new?state=" + url.QueryEscape(state)
	}
	return setupHTML(200, "Create the truner GitHub App", fmt.Sprintf(`
<p>GitHub will ask you to confirm creating a private GitHub App for <b>%s</b>. truner uses it to
register single-use runners and to receive <code>workflow_job</code> events. Its credentials are
stored in your AWS account.</p>
<form id="f" method="post" action="%s"><input type="hidden" name="manifest" value="%s">
<button type="submit">Continue to GitHub</button></form>
<script>setTimeout(function(){document.getElementById("f").submit()},800)</script>`,
		html.EscapeString(firstNonEmpty(owner, "your personal account")), html.EscapeString(target), html.EscapeString(string(manifest))))
}

func (cp *ControlPlane) setupCallback(ctx context.Context, req events.APIGatewayV2HTTPRequest) events.APIGatewayV2HTTPResponse {
	state, code := req.QueryStringParameters["state"], req.QueryStringParameters["code"]
	if state == "" || code == "" {
		return setupHTML(400, "Missing parameters", "<p>Open the link that <code>truner install</code> printed.</p>")
	}
	it, err := cp.claimSetup(ctx, state)
	if err != nil {
		cp.Log.Error("claim setup", "err", err)
		return setupHTML(500, "Something went wrong", "<p>Please try again in a moment.</p>")
	}
	if it == nil {
		return setupHTML(409, "This setup link was already used or has expired",
			"<p>Run <code>truner install</code> again to get a new link.</p>")
	}
	fail := func(msg string) events.APIGatewayV2HTTPResponse {
		cp.Log.Error("GitHub App setup failed", "err", msg)
		cp.Store.UpdateItem(ctx, setupPK, Item{"status": SetupFailed, "message": msg})
		return setupHTML(500, "The GitHub App could not be set up",
			"<p>"+html.EscapeString(msg)+"</p><p>Return to your terminal or agent and run <code>truner install</code> again.</p>")
	}
	creds, err := github.ConvertManifest(ctx, "", code)
	if err != nil {
		return fail("GitHub did not hand over the new app's credentials: " + err.Error())
	}
	if owner := it.Str("owner"); owner != "" && !strings.EqualFold(owner, creds.Owner) {
		return fail(fmt.Sprintf("the app %s belongs to %s, not %s. Delete it at %s", creds.Name, creds.Owner, owner, creds.HTMLURL))
	}
	replace, _ := it["replace"].(bool)
	if err := StoreAppCredentials(ctx, cp.SSM, cp.Env.AppParam, creds, replace); err != nil {
		if errors.Is(err, errAppExists) {
			return fail(fmt.Sprintf("%s. Delete the new app %s at %s, or run `truner install --new-github-app` to replace the old one", err, creds.Name, creds.HTMLURL))
		}
		return fail(err.Error())
	}
	cp.mu.Lock()
	cp.gh = nil
	cp.mu.Unlock()
	cp.Store.UpdateItem(ctx, setupPK, Item{"status": SetupCreated, "appName": creds.Name, "appSlug": creds.Slug, "appOwner": creds.Owner})
	cp.Log.Info("GitHub App created", "app", creds.Slug, "owner", creds.Owner)
	return redirect("https://github.com/apps/" + url.PathEscape(creds.Slug) + "/installations/new")
}

func installedPage(prefix string) events.APIGatewayV2HTTPResponse {
	return setupHTML(200, "✓ truner is connected to GitHub",
		"<p>The app is installed. You can close this tab: your terminal or AI agent continues on its own.</p>"+
			"<p>Change a job's <code>runs-on</code> to <code>"+html.EscapeString(prefix)+"/cpu=4/run=${{ github.run_id }}-build</code> "+
			"(with the job's own name after the run id) to run it on truner.</p>")
}

func redirect(to string) events.APIGatewayV2HTTPResponse {
	return events.APIGatewayV2HTTPResponse{StatusCode: 302, Headers: map[string]string{"Location": to, "Cache-Control": "no-store"}}
}

func setupHTML(status int, title, body string) events.APIGatewayV2HTTPResponse {
	page := `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex"><title>truner setup</title>
<style>body{font-family:system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;line-height:1.55;color:#1f2328}
code{font-size:.9em;background:#eff2f5;padding:.1em .35em;border-radius:4px}
button{font-size:1rem;padding:.6rem 1.2rem;border-radius:6px;border:1px solid #1f883d;background:#1f883d;color:#fff;cursor:pointer}
@media (prefers-color-scheme:dark){body{background:#0d1117;color:#e6edf3}code{background:#262c36}}</style></head>
<body><h1>` + html.EscapeString(title) + `</h1>` + body + `</body></html>`
	return events.APIGatewayV2HTTPResponse{
		StatusCode: status, Body: page,
		Headers: map[string]string{"Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store", "Referrer-Policy": "no-referrer"},
	}
}
