// Package version holds build information, set by the release build with -ldflags.
package version

var (
	// Version is the SemVer of this build ("dev" for local builds).
	Version = "dev"
	// Commit is the git commit the build came from.
	Commit = ""
	// Date is the build time, RFC 3339 UTC.
	Date = ""
)
