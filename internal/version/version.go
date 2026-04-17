// Package version exposes build-time metadata injected via -ldflags.
//
// Populate the variables with:
//
//	go build -ldflags "\
//	    -X github.com/clawmast/clawmast/internal/version.Version=v0.2.0 \
//	    -X github.com/clawmast/clawmast/internal/version.Commit=abc1234 \
//	    -X github.com/clawmast/clawmast/internal/version.BuildTime=2026-04-17T00:00:00Z"
//
// See the project Makefile for the canonical flag string.
package version

import "runtime"

var (
	// Version is the semver release string (for example "v0.2.0" or "dev").
	Version = "dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "none"
	// BuildTime is the RFC3339 UTC timestamp of the build.
	BuildTime = "unknown"
)

// Full returns a human-readable string combining the version fields and
// the Go runtime version. Suitable for the `version` subcommand and the
// /api/version endpoint.
func Full() string {
	return Version + " (" + Commit + ", " + BuildTime + ", " + runtime.Version() + ")"
}
