package cli

import (
	"strings"
	"testing"

	"github.com/codician-team/truner/internal/version"
)

func TestCLISkew(t *testing.T) {
	defer func(v string) { version.Version = v }(version.Version)
	version.Version = "v0.1.1"
	if older, msg := cliSkew("v0.1.3"); !older || !strings.Contains(msg, "older than the installation (v0.1.3)") || strings.Contains(msg, "truner upgrade") {
		t.Errorf("an older CLI must ask for a CLI update, never `truner upgrade`: %v %q", older, msg)
	}
	version.Version = "v0.1.3"
	if older, msg := cliSkew("v0.1.1"); older || !strings.Contains(msg, "run `truner upgrade`") {
		t.Errorf("a newer CLI upgrades the installation: %v %q", older, msg)
	}
	if older, msg := cliSkew("v0.1.3"); older || msg != "" {
		t.Errorf("same version: %v %q", older, msg)
	}
	if older, msg := cliSkew("dev-1a2b3c"); older || !strings.Contains(msg, "truner upgrade") {
		t.Errorf("a development installation is replaced by upgrade, as before: %v %q", older, msg)
	}
	version.Version = "dev"
	if _, msg := cliSkew("v0.1.3"); msg != "" {
		t.Errorf("development CLIs are not compared: %q", msg)
	}
}
