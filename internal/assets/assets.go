// Package assets gives the CLI access to the Lambda package and agent
// binaries it deploys. Release builds embed them (see tools/build); other
// builds download them from the matching GitHub release.
package assets

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/codician-team/truner/internal/version"
)

//go:embed all:dist
var dist embed.FS

// Names of the deployable artifacts.
const (
	LambdaZip         = "truner-lambda.zip"
	AgentLinuxAMD64   = "truner-agent-linux-amd64"
	AgentLinuxARM64   = "truner-agent-linux-arm64"
	AgentWindowsAMD64 = "truner-agent-windows-amd64.exe"
)

// All lists every artifact the control plane needs.
var All = []string{LambdaZip, AgentLinuxAMD64, AgentLinuxARM64, AgentWindowsAMD64}

// Embedded reports whether this binary carries the artifacts.
func Embedded() bool {
	_, err := fs.Stat(dist, "dist/"+LambdaZip)
	return err == nil
}

// Bundle holds the artifacts for one version.
type Bundle struct {
	Files map[string][]byte
	// Digest identifies the content; used to version development builds.
	Digest string
}

// Load returns the artifacts, from the binary when embedded, otherwise from
// $TRUNER_ASSETS_DIR or the GitHub release of this version.
func Load(ctx context.Context) (*Bundle, error) {
	b := &Bundle{Files: map[string][]byte{}}
	for _, name := range All {
		data, err := load(ctx, name)
		if err != nil {
			return nil, err
		}
		b.Files[name] = data
	}
	h := sha256.New()
	for _, name := range All {
		h.Write(b.Files[name])
	}
	b.Digest = hex.EncodeToString(h.Sum(nil))[:12]
	return b, nil
}

func load(ctx context.Context, name string) ([]byte, error) {
	if data, err := dist.ReadFile("dist/" + name); err == nil {
		return data, nil
	}
	if data, err := dist.ReadFile("dist/" + name + ".gz"); err == nil {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(zr)
	}
	if dir := os.Getenv("TRUNER_ASSETS_DIR"); dir != "" {
		return os.ReadFile(filepath.Join(dir, name))
	}
	if version.IsDev() {
		return nil, fmt.Errorf("this is a development build without embedded artifacts: run `go run ./tools/build` or set TRUNER_ASSETS_DIR")
	}
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", version.Repo, version.Version, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
