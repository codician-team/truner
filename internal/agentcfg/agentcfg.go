// Package agentcfg defines the configuration document the control plane
// hands to the on-instance agent through EC2 user data.
package agentcfg

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Agent modes.
const (
	ModeRun  = "run"  // run exactly one job, then terminate
	ModeBake = "bake" // install the profile, then stop so an AMI can be taken
	ModePool = "pool" // prepare, then stop and wait to be claimed
)

// UserDataMarker prefixes the config line embedded in user data.
const UserDataMarker = "TRUNER_CONFIG="

// Config is everything the agent needs to run.
type Config struct {
	Version  string `json:"version"`
	Mode     string `json:"mode"`
	Platform string `json:"platform"`
	Arch     string `json:"arch"`

	Stack  string `json:"stack"`
	Region string `json:"region"`
	Bucket string `json:"bucket"`

	// Runner registration
	RunnerName    string   `json:"runnerName,omitempty"`
	JITConfig     string   `json:"jitConfig,omitempty"`
	RunnerVersion string   `json:"runnerVersion,omitempty"`
	RunnerURLs    []string `json:"runnerUrls,omitempty"`

	// Image
	Profile   string `json:"profile"`
	ImageName string `json:"imageName,omitempty"`
	ImageOS   string `json:"imageOs,omitempty"`
	User      string `json:"user,omitempty"`

	// Features
	Cache       CacheConfig       `json:"cache"`
	ECRRegistry string            `json:"ecrRegistry,omitempty"`
	ECRRepo     string            `json:"ecrRepo,omitempty"`
	SSHUsers    []string          `json:"sshUsers,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	UseNVMe     bool              `json:"useNvme"`

	// Lifecycle
	IdleMinutes int `json:"idleMinutes"`
	MaxMinutes  int `json:"maxMinutes"`
	KeepMinutes int `json:"keepMinutes,omitempty"`

	// Reporting
	LogKey     string `json:"logKey,omitempty"`     // S3 key for the agent log
	MarkerKey  string `json:"markerKey,omitempty"`  // S3 key written on bake/pool completion
	FatalError string `json:"fatalError,omitempty"` // fail the job with this message
	Pool       string `json:"pool,omitempty"`

	Job JobInfo `json:"job"`
}

// CacheConfig configures the magic cache proxy.
type CacheConfig struct {
	Enabled bool   `json:"enabled"`
	Bucket  string `json:"bucket,omitempty"`
	Prefix  string `json:"prefix,omitempty"` // e.g. cache/owner/repo/
	Region  string `json:"region,omitempty"`
}

// JobInfo describes the job that triggered the launch, for the job log banner.
type JobInfo struct {
	Repo      string   `json:"repo,omitempty"`
	JobID     int64    `json:"jobId,omitempty"`
	RunID     int64    `json:"runId,omitempty"`
	Workflow  string   `json:"workflow,omitempty"`
	Name      string   `json:"name,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Spec      string   `json:"spec,omitempty"`
	QueuedAt  int64    `json:"queuedAt,omitempty"` // unix seconds
	Lifecycle string   `json:"lifecycle,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

// Encode serializes the config as gzip+base64 for embedding in user data.
func Encode(c *Config) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write(raw); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// Decode parses an Encode()d config.
func Decode(s string) (*Config, error) {
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	c := &Config{}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	return c, nil
}

// FromUserData extracts the config from a user data script.
func FromUserData(userData []byte) (*Config, error) {
	sc := bufio.NewScanner(bytes.NewReader(userData))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		// Accept shell (TRUNER_CONFIG=...) and PowerShell ($TRUNER_CONFIG="...") forms.
		line = strings.TrimPrefix(line, "$")
		if strings.HasPrefix(line, UserDataMarker) {
			v := strings.TrimPrefix(line, UserDataMarker)
			v = strings.Trim(v, `'"`)
			return Decode(v)
		}
	}
	return nil, fmt.Errorf("no %s line in user data", UserDataMarker)
}
