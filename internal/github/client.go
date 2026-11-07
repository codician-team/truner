// Package github is a small GitHub App client covering exactly the API
// surface truner needs. It has no third-party dependencies.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// DefaultAPI is the GitHub.com REST endpoint.
const DefaultAPI = "https://api.github.com"

// AppCredentials are the secrets of the truner GitHub App. They are stored as
// one JSON SecureString parameter in SSM.
type AppCredentials struct {
	AppID         int64  `json:"appId"`
	Slug          string `json:"slug"`
	Name          string `json:"name,omitempty"`
	ClientID      string `json:"clientId,omitempty"`
	ClientSecret  string `json:"clientSecret,omitempty"`
	WebhookSecret string `json:"webhookSecret"`
	PrivateKeyPEM string `json:"pem"`
	Owner         string `json:"owner,omitempty"`
	HTMLURL       string `json:"htmlUrl,omitempty"`
}

// Error is a non-2xx response from the API.
type Error struct {
	Status  int
	Method  string
	Path    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("github: %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// IsNotFound reports whether err is a 404 from the API.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// Client is a GitHub App client. It is safe for concurrent use.
type Client struct {
	BaseURL string
	HTTP    *http.Client

	app *AppCredentials
	key *rsa.PrivateKey

	mu     sync.Mutex
	tokens map[int64]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

// New returns a client authenticated as the given app.
func New(app *AppCredentials) (*Client, error) {
	c := &Client{BaseURL: DefaultAPI, HTTP: &http.Client{Timeout: 30 * time.Second}, tokens: map[int64]cachedToken{}}
	if v := os.Getenv("TRUNER_GITHUB_API"); v != "" {
		c.BaseURL = v // tests and GitHub Enterprise
	}
	if app != nil {
		key, err := ParsePrivateKey(app.PrivateKeyPEM)
		if err != nil {
			return nil, err
		}
		c.app, c.key = app, key
	}
	return c, nil
}

// App returns the credentials the client was built with.
func (c *Client) App() *AppCredentials { return c.app }

// ParsePrivateKey parses a PKCS#1 or PKCS#8 RSA private key.
func ParsePrivateKey(pemData string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, errors.New("github: invalid app private key (no PEM block)")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github: invalid app private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github: app private key is not RSA")
	}
	return rk, nil
}

// AppJWT returns a short-lived JWT authenticating as the app itself.
func (c *Client) AppJWT() (string, error) {
	if c.key == nil {
		return "", errors.New("github: client has no app credentials")
	}
	now := time.Now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(), // allow for clock drift
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": fmt.Sprint(c.app.AppID),
	})
	payload := base64.RawURLEncoding.EncodeToString(claims)
	signing := header + "." + payload
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// InstallationToken returns a (cached) installation access token.
func (c *Client) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	c.mu.Lock()
	if t, ok := c.tokens[installationID]; ok && time.Until(t.expires) > 5*time.Minute {
		c.mu.Unlock()
		return t.token, nil
	}
	c.mu.Unlock()
	jwt, err := c.AppJWT()
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.do(ctx, "Bearer "+jwt, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installationID), nil, &out); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.tokens[installationID] = cachedToken{token: out.Token, expires: out.ExpiresAt}
	c.mu.Unlock()
	return out.Token, nil
}

func (c *Client) asInstallation(ctx context.Context, installationID int64) (string, error) {
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return "", err
	}
	return "token " + tok, nil
}

func (c *Client) asApp() (string, error) {
	jwt, err := c.AppJWT()
	if err != nil {
		return "", err
	}
	return "Bearer " + jwt, nil
}

// do performs a JSON request. auth is a full Authorization header value or "".
func (c *Client) do(ctx context.Context, auth, method, path string, body, out any) error {
	_, err := c.doRaw(ctx, auth, method, path, body, out, "")
	return err
}

func (c *Client) doRaw(ctx context.Context, auth, method, path string, body, out any, accept string) ([]byte, error) {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = b
	}
	u := path
	if !strings.HasPrefix(path, "http") {
		u = strings.TrimRight(c.BaseURL, "/") + path
	}
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	var resp *http.Response
	for attempt := 0; ; attempt++ {
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rdr)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", accept)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "truner")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err = c.HTTP.Do(req)
		// Retry transport errors and gateway errors on idempotent requests only.
		idempotent := method == http.MethodGet || method == http.MethodDelete
		retryable := err != nil || resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout
		if !retryable || !idempotent || attempt >= 2 {
			if err != nil {
				return nil, err
			}
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
		}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(data))
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &e) == nil && e.Message != "" {
			msg = e.Message
		}
		return nil, &Error{Status: resp.StatusCode, Method: method, Path: path, Message: msg}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("github: decode %s: %w", path, err)
		}
	}
	return data, nil
}

// JITConfig is a just-in-time runner registration.
type JITConfig struct {
	Runner struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"runner"`
	EncodedJITConfig string `json:"encoded_jit_config"`
}

// GenerateJITConfig registers an ephemeral repository-level runner.
func (c *Client) GenerateJITConfig(ctx context.Context, installationID int64, owner, repo, name string, labels []string) (*JITConfig, error) {
	auth, err := c.asInstallation(ctx, installationID)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"name":            name,
		"runner_group_id": 1,
		"labels":          labels,
		"work_folder":     "_work",
	}
	out := &JITConfig{}
	err = c.do(ctx, auth, http.MethodPost, fmt.Sprintf("/repos/%s/%s/actions/runners/generate-jitconfig", owner, repo), body, out)
	return out, err
}

// Runner is a registered self-hosted runner.
type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // online | offline
	Busy   bool   `json:"busy"`
}

// GetRunner fetches a repository runner.
func (c *Client) GetRunner(ctx context.Context, installationID int64, owner, repo string, id int64) (*Runner, error) {
	auth, err := c.asInstallation(ctx, installationID)
	if err != nil {
		return nil, err
	}
	out := &Runner{}
	err = c.do(ctx, auth, http.MethodGet, fmt.Sprintf("/repos/%s/%s/actions/runners/%d", owner, repo, id), nil, out)
	return out, err
}

// DeleteRunner removes a repository runner. Missing runners are not an error.
func (c *Client) DeleteRunner(ctx context.Context, installationID int64, owner, repo string, id int64) error {
	auth, err := c.asInstallation(ctx, installationID)
	if err != nil {
		return err
	}
	err = c.do(ctx, auth, http.MethodDelete, fmt.Sprintf("/repos/%s/%s/actions/runners/%d", owner, repo, id), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// Job is a workflow job.
type Job struct {
	ID          int64    `json:"id"`
	RunID       int64    `json:"run_id"`
	RunAttempt  int      `json:"run_attempt"`
	Status      string   `json:"status"` // queued | in_progress | completed | waiting
	Conclusion  string   `json:"conclusion"`
	Name        string   `json:"name"`
	WorkflowNm  string   `json:"workflow_name"`
	Labels      []string `json:"labels"`
	RunnerName  string   `json:"runner_name"`
	HeadSHA     string   `json:"head_sha"`
	HeadBranch  string   `json:"head_branch"`
	CreatedAt   string   `json:"created_at"`
	StartedAt   string   `json:"started_at"`
	CompletedAt string   `json:"completed_at"`
	HTMLURL     string   `json:"html_url"`
}

// GetJob fetches a workflow job.
func (c *Client) GetJob(ctx context.Context, installationID int64, owner, repo string, id int64) (*Job, error) {
	auth, err := c.asInstallation(ctx, installationID)
	if err != nil {
		return nil, err
	}
	out := &Job{}
	err = c.do(ctx, auth, http.MethodGet, fmt.Sprintf("/repos/%s/%s/actions/jobs/%d", owner, repo, id), nil, out)
	return out, err
}

// Run is a workflow run, as of its latest attempt.
type Run struct {
	ID         int64  `json:"id"`
	RunAttempt int    `json:"run_attempt"`
	Status     string `json:"status"` // queued | in_progress | completed | ...
	Conclusion string `json:"conclusion"`
}

// GetRun fetches a workflow run.
func (c *Client) GetRun(ctx context.Context, installationID int64, owner, repo string, id int64) (*Run, error) {
	auth, err := c.asInstallation(ctx, installationID)
	if err != nil {
		return nil, err
	}
	out := &Run{}
	err = c.do(ctx, auth, http.MethodGet, fmt.Sprintf("/repos/%s/%s/actions/runs/%d", owner, repo, id), nil, out)
	return out, err
}

// RerunJob re-runs a single job (and its dependents).
func (c *Client) RerunJob(ctx context.Context, installationID int64, owner, repo string, id int64) error {
	auth, err := c.asInstallation(ctx, installationID)
	if err != nil {
		return err
	}
	return c.do(ctx, auth, http.MethodPost, fmt.Sprintf("/repos/%s/%s/actions/jobs/%d/rerun", owner, repo, id), map[string]any{}, nil)
}

// GetFile returns the raw contents of a file. A missing file returns (nil, nil).
func (c *Client) GetFile(ctx context.Context, installationID int64, owner, repo, path, ref string) ([]byte, error) {
	auth, err := c.asInstallation(ctx, installationID)
	if err != nil {
		return nil, err
	}
	p := fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repo, path)
	if ref != "" {
		p += "?ref=" + url.QueryEscape(ref)
	}
	data, err := c.doRaw(ctx, auth, http.MethodGet, p, nil, nil, "application/vnd.github.raw+json")
	if IsNotFound(err) {
		return nil, nil
	}
	return data, err
}

// Installation is an installation of the app.
type Installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	RepositorySelection string            `json:"repository_selection"`
	Permissions         map[string]string `json:"permissions"`
	HTMLURL             string            `json:"html_url"`
	SuspendedAt         *string           `json:"suspended_at"`
}

// ListInstallations lists the app's installations.
func (c *Client) ListInstallations(ctx context.Context) ([]Installation, error) {
	auth, err := c.asApp()
	if err != nil {
		return nil, err
	}
	var out []Installation
	err = c.do(ctx, auth, http.MethodGet, "/app/installations?per_page=100", nil, &out)
	return out, err
}

// AppInfo is the public description of the app.
type AppInfo struct {
	ID          int64             `json:"id"`
	Slug        string            `json:"slug"`
	Name        string            `json:"name"`
	HTMLURL     string            `json:"html_url"`
	Permissions map[string]string `json:"permissions"`
	Events      []string          `json:"events"`
	Owner       struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// GetApp returns the authenticated app.
func (c *Client) GetApp(ctx context.Context) (*AppInfo, error) {
	auth, err := c.asApp()
	if err != nil {
		return nil, err
	}
	out := &AppInfo{}
	err = c.do(ctx, auth, http.MethodGet, "/app", nil, out)
	return out, err
}

// HookDelivery is a webhook delivery attempt.
type HookDelivery struct {
	ID          int64  `json:"id"`
	GUID        string `json:"guid"`
	DeliveredAt string `json:"delivered_at"`
	Redelivery  bool   `json:"redelivery"`
	Status      string `json:"status"`
	StatusCode  int    `json:"status_code"`
	Event       string `json:"event"`
	Action      string `json:"action"`
	Duration    float64
}

// ListHookDeliveries lists recent webhook deliveries of the app.
func (c *Client) ListHookDeliveries(ctx context.Context, perPage int) ([]HookDelivery, error) {
	auth, err := c.asApp()
	if err != nil {
		return nil, err
	}
	var out []HookDelivery
	err = c.do(ctx, auth, http.MethodGet, fmt.Sprintf("/app/hook/deliveries?per_page=%d", perPage), nil, &out)
	return out, err
}

// RedeliverHook asks GitHub to redeliver a webhook.
func (c *Client) RedeliverHook(ctx context.Context, id int64) error {
	auth, err := c.asApp()
	if err != nil {
		return err
	}
	return c.do(ctx, auth, http.MethodPost, fmt.Sprintf("/app/hook/deliveries/%d/attempts", id), map[string]any{}, nil)
}

// UpdateHookConfig points the app webhook at a new URL.
func (c *Client) UpdateHookConfig(ctx context.Context, hookURL, secret string) error {
	auth, err := c.asApp()
	if err != nil {
		return err
	}
	body := map[string]any{"url": hookURL, "content_type": "json"}
	if secret != "" {
		body["secret"] = secret
	}
	return c.do(ctx, auth, http.MethodPatch, "/app/hook/config", body, nil)
}

// Release is a GitHub release.
type Release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

// LatestRelease returns the latest release of a public repository. When
// installationID is non-zero the request is authenticated for a higher rate limit.
func (c *Client) LatestRelease(ctx context.Context, installationID int64, ownerRepo string) (*Release, error) {
	auth := ""
	if installationID != 0 && c.key != nil {
		a, err := c.asInstallation(ctx, installationID)
		if err == nil {
			auth = a
		}
	}
	out := &Release{}
	err := c.do(ctx, auth, http.MethodGet, "/repos/"+ownerRepo+"/releases/latest", nil, out)
	return out, err
}

// ConvertManifest exchanges a manifest-flow code for the new app's credentials.
func ConvertManifest(ctx context.Context, baseURL, code string) (*AppCredentials, error) {
	c := &Client{BaseURL: baseURL, HTTP: &http.Client{Timeout: 30 * time.Second}}
	if c.BaseURL == "" {
		c.BaseURL = DefaultAPI
		if v := os.Getenv("TRUNER_GITHUB_API"); v != "" {
			c.BaseURL = v
		}
	}
	var out struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		Name          string `json:"name"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
		WebhookSecret string `json:"webhook_secret"`
		PEM           string `json:"pem"`
		HTMLURL       string `json:"html_url"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	if err := c.do(ctx, "", http.MethodPost, "/app-manifests/"+url.PathEscape(code)+"/conversions", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &AppCredentials{
		AppID: out.ID, Slug: out.Slug, Name: out.Name, ClientID: out.ClientID, ClientSecret: out.ClientSecret,
		WebhookSecret: out.WebhookSecret, PrivateKeyPEM: out.PEM, HTMLURL: out.HTMLURL, Owner: out.Owner.Login,
	}, nil
}

// UserKeys returns the public SSH keys of a GitHub user.
func UserKeys(ctx context.Context, login string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://github.com/"+url.PathEscape(login)+".keys", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: keys for %s: %s", login, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			keys = append(keys, l)
		}
	}
	return keys, nil
}
