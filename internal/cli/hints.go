package cli

import (
	"fmt"
	"runtime"

	"github.com/codician-team/truner/internal/version"
)

// cliSkew compares this CLI with the installation's version. older is true
// when the CLI is older and may not know the installation's labels, images
// and checks; msg says what to do, or is "" when the versions match.
func cliSkew(installed string) (older bool, msg string) {
	if installed == "" || installed == version.Version || version.IsDev() {
		return false, ""
	}
	if c, ok := version.Compare(version.Version, installed); ok && c < 0 {
		return true, fmt.Sprintf("this CLI is %s, older than the installation (%s): it may not know the installation's newer labels, images and checks. Update it: %s",
			version.Version, installed, cliInstallCommand())
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
