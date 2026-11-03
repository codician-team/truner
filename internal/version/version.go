// Package version holds build metadata injected at link time.
package version

import "fmt"

// These are overridden with -ldflags "-X github.com/codician-team/truner/internal/version.Version=..."
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
	// Repo is the GitHub "owner/name" slug releases are published under.
	Repo = "codician-team/truner"
)

// String returns a human readable version line.
func String() string {
	return fmt.Sprintf("truner %s (commit %s, built %s)", Version, Commit, Date)
}

// IsDev reports whether this is a local development build.
func IsDev() bool { return Version == "dev" }
