//go:build !failstartup

package main

import "log/slog"

// maybeSimulateStartupFailure is a no-op in production builds.
//
// The companion file fail_startup_on.go provides a real
// implementation behind the `failstartup` build tag. Smoke tests
// that exercise the supervisor's install HealthGate compile the
// worker with `-tags failstartup` and set CLAWMAST_FAIL_MODE to
// pick a specific failure shape. Keeping the failure surface
// behind a build tag means production artefacts never carry the
// code path, and a misconfigured env var in prod simply does
// nothing.
func maybeSimulateStartupFailure(_ *slog.Logger) {}
