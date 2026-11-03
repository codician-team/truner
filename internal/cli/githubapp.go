package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/codician-team/truner/internal/controlplane"
	"github.com/codician-team/truner/internal/github"
)

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

// connectApp creates the GitHub App through the control plane's setup page
// (see controlplane/setup.go) and waits until it exists. Nothing on this
// machine has to receive GitHub's redirect, so the link can be opened on any
// device. A link that is still pending from an interrupted run is reused, so
// running `truner install` again simply continues.
func connectApp(ctx context.Context, u *ui, e *env, owner string, replace bool) (*controlplane.SetupStatus, error) {
	key := e.cfg.Region + "/" + e.name
	st := loadState()
	pending, reuse := st.Setups[key]
	if reuse && (pending.Owner != owner || pending.Replace != replace) {
		reuse = false
	}
	if reuse {
		s, err := setupStatus(ctx, e, pending.State)
		reuse = err == nil && (s.Status == controlplane.SetupPending || s.Status == controlplane.SetupCreated)
		if reuse && s.Status == controlplane.SetupCreated {
			forgetSetup(key)
			return s, nil
		}
	}
	if !reuse {
		b := make([]byte, 24)
		rand.Read(b)
		pending = pendingSetup{State: hex.EncodeToString(b), Owner: owner, Replace: replace}
		resp, err := e.invoke(ctx, controlplane.AdminRequest{
			Truner: "github-setup", State: pending.State, Owner: owner, AppName: appName(owner, e.name), Replace: replace,
		}, false)
		if err != nil {
			return nil, err
		}
		if !resp.OK {
			return nil, errors.New(resp.Message)
		}
		if st.Setups == nil {
			st.Setups = map[string]pendingSetup{}
		}
		st.Setups[key] = pending
		saveState(st)
	}

	link := strings.TrimSuffix(e.out("WebhookUrl"), "/webhook") + "/github/setup?state=" + pending.State
	u.action(`Open this link, click "Create GitHub App", then choose the repositories that should use truner:`, link)
	u.info("%s", u.dim("The link works on any device and stays valid for an hour. If this command stops, run it again to continue."))
	openBrowser(link)

	sp := u.spin("Waiting for the GitHub App to be created")
	defer sp.done()
	hb := u.heartbeat()
	deadline := time.Now().Add(controlplane.SetupTTL + time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
		s, err := setupStatus(ctx, e, pending.State)
		if err != nil {
			continue
		}
		switch s.Status {
		case controlplane.SetupCreated:
			forgetSetup(key)
			return s, nil
		case controlplane.SetupFailed:
			forgetSetup(key)
			return nil, errors.New(s.Message)
		case controlplane.SetupExpired:
			forgetSetup(key)
			return nil, fmt.Errorf("%s; run `truner install` again for a new link", s.Message)
		}
		hb("waiting for the GitHub App to be created")
	}
	return nil, errors.New("timed out waiting for the GitHub App; run `truner install` again for a new link")
}

func setupStatus(ctx context.Context, e *env, state string) (*controlplane.SetupStatus, error) {
	resp, err := e.invoke(ctx, controlplane.AdminRequest{Truner: "github-setup-status", State: state}, false)
	if err != nil {
		return nil, err
	}
	if !resp.OK || resp.Setup == nil {
		return nil, errors.New(firstNonEmpty(resp.Message, "no setup status"))
	}
	return resp.Setup, nil
}

func forgetSetup(key string) {
	st := loadState()
	if _, ok := st.Setups[key]; ok {
		delete(st.Setups, key)
		saveState(st)
	}
}

// waitForInstallation waits until the app is installed somewhere. When the
// browser is already on the installation page (after creating the app) it
// is not opened again.
func waitForInstallation(ctx context.Context, u *ui, gh *github.Client, slug string, open bool) []github.Installation {
	insts, err := gh.ListInstallations(ctx)
	if err == nil && len(insts) > 0 {
		return insts
	}
	url := "https://github.com/apps/" + slug + "/installations/new"
	if open {
		u.action("Install the GitHub App on the repositories that should use truner:", url)
		openBrowser(url)
	} else {
		u.action(`The browser now shows the app's installation page. Choose the repositories and click "Install":`, url)
	}
	sp := u.spin("Waiting for the installation (Ctrl-C to finish later)")
	defer sp.done()
	hb := u.heartbeat()
	deadline := time.Now().Add(30 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(4 * time.Second):
		}
		if insts, err := gh.ListInstallations(ctx); err == nil && len(insts) > 0 {
			return insts
		}
		hb("waiting for the GitHub App to be installed")
	}
	return nil
}
