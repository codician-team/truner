// Package version holds build metadata injected at link time.
package version

import (
	"fmt"
	"strconv"
	"strings"
)

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

// Compare orders two release versions such as v0.1.3 by their numbers: -1 when
// a is older than b, 0 when equal, 1 when newer. ok is false when either is not
// a release version (development builds) or when they differ only in a suffix
// such as `git describe`'s -3-gabc123, whose order is unknown.
func Compare(a, b string) (c int, ok bool) {
	na, sa, okA := parse(a)
	nb, sb, okB := parse(b)
	if !okA || !okB {
		return 0, false
	}
	for i := range na {
		if na[i] != nb[i] {
			if na[i] < nb[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, sa == sb
}

func parse(v string) (nums [3]int, suffix string, ok bool) {
	core, ok := strings.CutPrefix(v, "v")
	if !ok {
		return nums, "", false
	}
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core, suffix = core[:i], core[i:]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return nums, "", false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nums, "", false
		}
		nums[i] = n
	}
	return nums, suffix, true
}
