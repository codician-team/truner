// Command build produces release artifacts:
//
//	go run ./tools/build                       # all platforms, version from git
//	go run ./tools/build -version v1.2.3       # explicit version
//	go run ./tools/build -targets host         # only the CLI for this machine
//
// It compiles the agent (linux/amd64, linux/arm64, windows/amd64) and the
// control plane Lambda (linux/arm64), embeds them into the CLI and writes
// CLI binaries, release assets and checksums to dist/.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const pkg = "github.com/truner/truner"

var cliTargets = [][2]string{
	{"linux", "amd64"}, {"linux", "arm64"},
	{"darwin", "amd64"}, {"darwin", "arm64"},
	{"windows", "amd64"}, {"windows", "arm64"},
}

func main() {
	ver := flag.String("version", "", "version (default: git describe)")
	targets := flag.String("targets", "all", "CLI targets: all or host")
	out := flag.String("out", "dist", "output directory")
	repo := flag.String("repo", "", "GitHub owner/name releases are published under (default: from version package)")
	flag.Parse()
	if *ver == "" {
		*ver = gitVersion()
	}
	commit := strings.TrimSpace(run("git", "rev-parse", "--short", "HEAD"))
	if commit == "" {
		commit = "none"
	}
	ldflags := fmt.Sprintf("-s -w -X %[1]s/internal/version.Version=%[2]s -X %[1]s/internal/version.Commit=%[3]s -X %[1]s/internal/version.Date=%[4]s",
		pkg, *ver, commit, time.Now().UTC().Format(time.RFC3339))
	if *repo != "" {
		ldflags += fmt.Sprintf(" -X %s/internal/version.Repo=%s", pkg, *repo)
	}
	must(os.MkdirAll(*out, 0o755))
	tmp, err := os.MkdirTemp("", "truner-build-*")
	must(err)
	defer os.RemoveAll(tmp)
	assetDir := filepath.Join("internal", "assets", "dist")
	cleanAssets(assetDir)

	fmt.Printf("building truner %s (%s)\n", *ver, commit)
	agents := map[string][2]string{
		"truner-agent-linux-amd64":       {"linux", "amd64"},
		"truner-agent-linux-arm64":       {"linux", "arm64"},
		"truner-agent-windows-amd64.exe": {"windows", "amd64"},
	}
	names := make([]string, 0, len(agents))
	for n := range agents {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		t := agents[name]
		bin := filepath.Join(tmp, name)
		gobuild(t[0], t[1], ldflags, "", bin, "./cmd/truner-agent")
		data, err := os.ReadFile(bin)
		must(err)
		must(os.WriteFile(filepath.Join(assetDir, name+".gz"), gz(data), 0o644))
		must(os.WriteFile(filepath.Join(*out, name), data, 0o755))
		fmt.Printf("  agent   %-32s %s\n", name, size(len(data)))
	}
	lambdaBin := filepath.Join(tmp, "bootstrap")
	gobuild("linux", "arm64", ldflags, "lambda.norpc", lambdaBin, "./cmd/truner-lambda")
	zipped := lambdaZip(lambdaBin)
	must(os.WriteFile(filepath.Join(assetDir, "truner-lambda.zip"), zipped, 0o644))
	must(os.WriteFile(filepath.Join(*out, "truner-lambda.zip"), zipped, 0o644))
	fmt.Printf("  lambda  %-32s %s\n", "truner-lambda.zip", size(len(zipped)))

	list := cliTargets
	if *targets == "host" {
		list = [][2]string{{runtime.GOOS, runtime.GOARCH}}
	}
	var files []string
	for _, t := range list {
		name := fmt.Sprintf("truner_%s_%s", t[0], t[1])
		if t[0] == "windows" {
			name += ".exe"
		}
		bin := filepath.Join(*out, name)
		gobuild(t[0], t[1], ldflags, "", bin, "./cmd/truner")
		st, _ := os.Stat(bin)
		fmt.Printf("  cli     %-32s %s\n", name, size(int(st.Size())))
		files = append(files, name)
		if *targets == "all" {
			files = append(files, archive(*out, name, t[0], *ver))
		}
	}
	files = append(files, "truner-lambda.zip")
	files = append(files, names...)
	writeChecksums(*out, files)
	fmt.Printf("done: %s\n", *out)
}

func gitVersion() string {
	v := strings.TrimSpace(run("git", "describe", "--tags", "--always", "--dirty"))
	if v == "" {
		return "v0.0.0-dev"
	}
	if !strings.HasPrefix(v, "v") {
		v = "v0.0.0-" + v
	}
	return v
}

func gobuild(goos, goarch, ldflags, tags, out, pkgPath string) {
	args := []string{"build", "-trimpath", "-ldflags", ldflags, "-o", out}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, pkgPath)
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "build %s/%s %s failed: %v\n", goos, goarch, pkgPath, err)
		os.Exit(1)
	}
}

func lambdaZip(bin string) []byte {
	data, err := os.ReadFile(bin)
	must(err)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: "bootstrap", Method: zip.Deflate}
	h.SetMode(0o755)
	h.Modified = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	w, err := zw.CreateHeader(h)
	must(err)
	_, err = w.Write(data)
	must(err)
	must(zw.Close())
	return buf.Bytes()
}

func gz(data []byte) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	zw.Write(data)
	zw.Close()
	return buf.Bytes()
}

// archive packs a CLI binary as truner_<ver>_<os>_<arch>.tar.gz/.zip.
func archive(dir, name, goos, ver string) string {
	data, err := os.ReadFile(filepath.Join(dir, name))
	must(err)
	base := strings.TrimSuffix(name, ".exe")
	base = strings.Replace(base, "truner_", "truner_"+strings.TrimPrefix(ver, "v")+"_", 1)
	exe := "truner"
	if goos == "windows" {
		exe = "truner.exe"
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		h := &zip.FileHeader{Name: exe, Method: zip.Deflate}
		h.SetMode(0o755)
		w, _ := zw.CreateHeader(h)
		w.Write(data)
		zw.Close()
		must(os.WriteFile(filepath.Join(dir, base+".zip"), buf.Bytes(), 0o644))
		return base + ".zip"
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: exe, Mode: 0o755, Size: int64(len(data)), ModTime: time.Now()})
	tw.Write(data)
	tw.Close()
	zw.Close()
	must(os.WriteFile(filepath.Join(dir, base+".tar.gz"), buf.Bytes(), 0o644))
	return base + ".tar.gz"
}

func writeChecksums(dir string, files []string) {
	var b strings.Builder
	sort.Strings(files)
	for _, f := range files {
		fh, err := os.Open(filepath.Join(dir, f))
		must(err)
		h := sha256.New()
		io.Copy(h, fh)
		fh.Close()
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(h.Sum(nil)), f)
	}
	must(os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(b.String()), 0o644))
}

func cleanAssets(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != ".keep" {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func run(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func size(n int) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
