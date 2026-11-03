package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/truner/truner/internal/config"
	"github.com/truner/truner/internal/github"
)

const runnerPK = "runner#latest"

type runnerRelease struct {
	Version  string
	Mirrored map[string]bool
}

// RunnerRelease returns the actions/runner version to install, preferring the
// state maintained by the janitor and falling back to asking GitHub.
func (cp *ControlPlane) RunnerRelease(ctx context.Context) (*runnerRelease, error) {
	cp.mu.Lock()
	if cp.runnerRel != nil && time.Since(cp.runnerAt) < 5*time.Minute {
		r := cp.runnerRel
		cp.mu.Unlock()
		return r, nil
	}
	cp.mu.Unlock()
	rel := &runnerRelease{Mirrored: map[string]bool{}}
	it, err := cp.Store.GetItem(ctx, runnerPK)
	if err != nil {
		return nil, err
	}
	if it != nil && it.Str("version") != "" {
		rel.Version = it.Str("version")
		for _, f := range strings.Split(it.Str("mirrored"), ",") {
			if f != "" {
				rel.Mirrored[f] = true
			}
		}
	} else {
		v, err := cp.latestRunnerVersion(ctx)
		if err != nil {
			return nil, err
		}
		rel.Version = v
	}
	cp.mu.Lock()
	cp.runnerRel, cp.runnerAt = rel, time.Now()
	cp.mu.Unlock()
	return rel, nil
}

func (cp *ControlPlane) latestRunnerVersion(ctx context.Context) (string, error) {
	gh, err := cp.GitHub(ctx)
	if err != nil {
		gh, _ = github.New(nil)
	}
	var inst int64
	if gh.App() != nil {
		if insts, err := gh.ListInstallations(ctx); err == nil && len(insts) > 0 {
			inst = insts[0].ID
		}
	}
	r, err := gh.LatestRelease(ctx, inst, "actions/runner")
	if err != nil {
		return "", fmt.Errorf("latest runner release: %w", err)
	}
	return strings.TrimPrefix(r.TagName, "v"), nil
}

// MirrorRunner keeps an in-region copy of the latest runner packages in S3 so
// instances download ~200 MB at S3 speed instead of from GitHub.
func (cp *ControlPlane) MirrorRunner(ctx context.Context, force bool) error {
	it, err := cp.Store.GetItem(ctx, runnerPK)
	if err != nil {
		return err
	}
	if it != nil && !force && time.Since(time.Unix(it.Int("checkedAt"), 0)) < 6*time.Hour {
		return nil
	}
	v, err := cp.latestRunnerVersion(ctx)
	if err != nil {
		return err
	}
	var mirrored []string
	var errs []error
	for _, pa := range [][2]string{{config.PlatformLinux, config.ArchX64}, {config.PlatformLinux, config.ArchARM64}, {config.PlatformWindows, config.ArchX64}} {
		file := runnerFile(v, pa[0], pa[1])
		key := "runners/" + file
		if _, err := cp.S3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &cp.Env.Bucket, Key: &key}); err == nil {
			mirrored = append(mirrored, file)
			continue
		}
		if err := cp.mirrorFile(ctx, fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/%s", v, file), key); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", file, err))
			continue
		}
		mirrored = append(mirrored, file)
		cp.Log.Info("mirrored runner package", "file", file)
	}
	if err := cp.Store.UpdateItem(ctx, runnerPK, Item{"version": v, "mirrored": strings.Join(mirrored, ","), "checkedAt": time.Now().Unix()}); err != nil {
		return err
	}
	cp.mu.Lock()
	cp.runnerRel = nil
	cp.mu.Unlock()
	return errors.Join(errs...)
}

func (cp *ControlPlane) mirrorFile(ctx context.Context, url, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 4 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: %s", resp.Status)
	}
	up := manager.NewUploader(cp.S3, func(u *manager.Uploader) { u.PartSize = 16 << 20; u.Concurrency = 4 })
	_, err = up.Upload(ctx, &s3.PutObjectInput{Bucket: &cp.Env.Bucket, Key: aws.String(key), Body: resp.Body})
	return err
}
