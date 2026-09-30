// Package version reports the build version of the IDDC binaries.
//
// Release builds stamp the variables below with -ldflags (see the Makefile and
// scripts/release.sh). Binaries built with `go install …@vX.Y.Z` fall back to
// the module version recorded by the Go toolchain.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

var (
	// Version is the semantic version, e.g. "v0.2.0".
	Version = ""
	// Commit is the git commit the binary was built from.
	Commit = ""
	// Date is the UTC build timestamp (RFC 3339).
	Date = ""
)

// Get returns the version, falling back to Go build info and then "dev".
func Get() string {
	if Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

// String returns a one-line description such as
// "v0.2.0 (commit 1a2b3c4, built 2026-09-30T10:00:00Z, go1.26.8 darwin/arm64)".
func String() string {
	s := Get()
	var details []interface{}
	format := ""
	if Commit != "" {
		format += "commit %s, "
		details = append(details, Commit)
	}
	if Date != "" {
		format += "built %s, "
		details = append(details, Date)
	}
	format += "%s %s/%s"
	details = append(details, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return s + " (" + fmt.Sprintf(format, details...) + ")"
}
