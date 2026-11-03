package cli

import (
	"fmt"
	"runtime"

	"github.com/codician-team/truner/internal/labels"
	"github.com/codician-team/truner/internal/version"
)

// labelPrefix is the first runs-on label this installation answers to. Every
// example label the CLI prints starts with it: a job written for another
// prefix waits for a machine that never comes.
func (e *env) labelPrefix() string {
	return firstNonEmpty(e.stack.Parameters["LabelPrefix"], labels.DefaultPrefix)
}

// exampleLabel is a runs-on label for a job named job, with a run= token of
// its own so that no other job takes its machine.
func exampleLabel(prefix, settings, job string) string {
	return fmt.Sprintf("%s/%s/run=${{ github.run_id }}-%s", prefix, settings, job)
}

// cliSkew compares this CLI with the installation's version. older is true
// when the CLI is older and may not know the installation's labels, images
// and checks; msg says what to do, or is "" when the versions match.
func cliSkew(installed string) (older bool, msg string) {
	if installed == "" || installed == version.Version || version.IsDev() {
		return false, ""
	}
	if c, ok := version.Compare(version.Version, installed); ok && c < 0 {
		return true, fmt.Sprintf("this CLI is %s, older than the installation (%s): it may not know the installation's newer labels, images and checks. Update it: truner upgrade --cli-only",
			version.Version, installed)
	}
	return false, fmt.Sprintf("installation runs %s but this CLI is %s - run `truner upgrade`", installed, version.Version)
}

// cliInstallCommand installs the latest CLI release.
func cliInstallCommand() string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("irm https://raw.githubusercontent.com/%s/main/install.ps1 | iex", version.Repo)
	}
	return fmt.Sprintf("curl -fsSL https://raw.githubusercontent.com/%s/main/install.sh | sh", version.Repo)
}
