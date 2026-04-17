//go:build windows

package main

import "context"

// run is a Windows stub. ClawMast supervisor is Tier 1
// (macOS/Linux) only per AGENTS.md R5 and
// architecture/client-topology.md §2. On Windows the worker is
// expected to be launched directly by the user; clawmastd exists on
// this platform only so the cross-platform `version` subcommand keeps
// working.
func run(ctx context.Context, opts options) error {
	_ = ctx
	_ = opts
	return &exitError{
		code:   2,
		reason: "clawmastd supervisor is not supported on windows; run clawmast.exe directly",
	}
}
