package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// VerifySignature checks an X-Hub-Signature-256 header against the body.
func VerifySignature(secret string, body []byte, header string) bool {
	if secret == "" || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// Sign computes an X-Hub-Signature-256 header value (used in tests and tooling).
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// WorkflowJobEvent is the payload of a `workflow_job` webhook.
type WorkflowJobEvent struct {
	Action      string `json:"action"` // queued | waiting | in_progress | completed
	WorkflowJob struct {
		ID              int64      `json:"id"`
		RunID           int64      `json:"run_id"`
		RunAttempt      int        `json:"run_attempt"`
		Name            string     `json:"name"`
		WorkflowName    string     `json:"workflow_name"`
		HeadBranch      string     `json:"head_branch"`
		HeadSHA         string     `json:"head_sha"`
		Status          string     `json:"status"`
		Conclusion      string     `json:"conclusion"`
		Labels          []string   `json:"labels"`
		RunnerID        int64      `json:"runner_id"`
		RunnerName      string     `json:"runner_name"`
		RunnerGroupName string     `json:"runner_group_name"`
		CreatedAt       *time.Time `json:"created_at"`
		StartedAt       *time.Time `json:"started_at"`
		CompletedAt     *time.Time `json:"completed_at"`
		HTMLURL         string     `json:"html_url"`
	} `json:"workflow_job"`
	Repository struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		Private  bool   `json:"private"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}
