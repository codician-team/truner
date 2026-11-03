package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/codician-team/truner/internal/github"
	"github.com/codician-team/truner/internal/version"
)

// appPermissions are what truner needs, and why:
//   - administration:write registers ephemeral runners on each repository
//   - actions:write reads jobs and re-runs jobs killed by spot interruptions
//   - contents:read reads .github/truner.yml
var appPermissions = map[string]string{
	"administration": "write",
	"actions":        "write",
	"contents":       "read",
	"metadata":       "read",
}

func appName(owner, stack string) string {
	n := "truner"
	if owner != "" {
		n += "-" + strings.ToLower(owner)
	} else {
		b := make([]byte, 3)
		rand.Read(b)
		n += "-" + hex.EncodeToString(b)
	}
	if stack != "truner" {
		n += "-" + stack
	}
	if len(n) > 34 {
		n = n[:34]
	}
	return strings.Trim(n, "-")
}

// createApp runs GitHub's app-manifest flow: a local page posts the manifest
// to GitHub, the user confirms, and GitHub redirects back with a code that
// is exchanged for the new app's credentials.
func createApp(ctx context.Context, u *ui, owner, stack, webhookURL string) (*github.AppCredentials, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	stateBytes := make([]byte, 16)
	rand.Read(stateBytes)
	state := hex.EncodeToString(stateBytes)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	manifest := map[string]any{
		"name":                appName(owner, stack),
		"url":                 "https://github.com/" + version.Repo,
		"description":         "Ephemeral GitHub Actions runners in our own AWS account, powered by truner.",
		"hook_attributes":     map[string]any{"url": webhookURL, "active": true},
		"redirect_url":        base + "/callback",
		"public":              false,
		"default_permissions": appPermissions,
		"default_events":      []string{"workflow_job"},
	}
	mj, _ := json.Marshal(manifest)
	target := "https://github.com/settings/apps/new?state=" + state
	if owner != "" {
		target = "https://github.com/organizations/" + owner + "/settings/apps/new?state=" + state
	}

	type result struct {
		creds *github.AppCredentials
		err   error
	}
	done := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>truner setup</title>
<style>body{font-family:system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;color:#1f2328}button{font-size:1rem;padding:.6rem 1.2rem;border-radius:6px;border:1px solid #1f883d;background:#1f883d;color:#fff;cursor:pointer}</style></head>
<body><h1>Create the truner GitHub App</h1>
<p>GitHub will ask you to confirm creating a private GitHub App for <b>%s</b>. truner uses it to register ephemeral runners and receive <code>workflow_job</code> events.</p>
<form id="f" method="post" action="%s"><input type="hidden" name="manifest" value="%s"><button type="submit">Continue to GitHub</button></form>
<script>setTimeout(function(){document.getElementById("f").submit()},800)</script></body></html>`,
			html.EscapeString(firstNonEmpty(owner, "your account")), html.EscapeString(target), html.EscapeString(string(mj)))
	})
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch, please restart `truner install`", http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		creds, err := github.ConvertManifest(r.Context(), "", code)
		if err != nil {
			fmt.Fprintf(w, "<h1>Something went wrong</h1><pre>%s</pre><p>Return to the terminal.</p>", html.EscapeString(err.Error()))
			done <- result{err: err}
			return
		}
		fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>truner setup</title>
<style>body{font-family:system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem}</style></head>
<body><h1>✓ GitHub App created</h1><p>Next, install <b>%s</b> on the repositories that should use truner:</p>
<p><a href="https://github.com/apps/%s/installations/new">Install the app</a></p><p>You can then return to the terminal.</p></body></html>`, html.EscapeString(creds.Name), html.EscapeString(creds.Slug))
		done <- result{creds: creds}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	u.info("Opening your browser to create the GitHub App...")
	u.info("If it does not open, visit %s", u.cyan(base))
	openBrowser(base)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	sp := u.spin("Waiting for you to confirm on GitHub")
	defer sp.done()
	select {
	case r := <-done:
		return r.creds, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("timed out waiting for the GitHub App to be created")
	}
}

// waitForInstallation waits until the app is installed somewhere.
func waitForInstallation(ctx context.Context, u *ui, gh *github.Client, slug string) []github.Installation {
	insts, err := gh.ListInstallations(ctx)
	if err == nil && len(insts) > 0 {
		return insts
	}
	url := "https://github.com/apps/" + slug + "/installations/new"
	u.info("Install the app on the repositories that should use truner:")
	u.info("  %s", u.cyan(url))
	openBrowser(url)
	sp := u.spin("Waiting for the installation (Ctrl-C to finish later)")
	defer sp.done()
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(4 * time.Second):
		}
		if insts, err := gh.ListInstallations(ctx); err == nil && len(insts) > 0 {
			return insts
		}
	}
	return nil
}
