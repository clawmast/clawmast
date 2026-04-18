//go:build failstartup

package main

import (
	"fmt"
	"log/slog"
	"os"
)

// maybeSimulateStartupFailure implements the two failure shapes the
// supervisor's install HealthGate has to survive:
//
//   - CLAWMAST_FAIL_MODE=crash  →  exit(1) before READY, exercising
//     the "one crash inside the window" branch that produces
//     GateFailReasonCrash.
//   - CLAWMAST_FAIL_MODE=hang   →  block forever without sending
//     READY, exercising the deadline-elapsed branch that produces
//     GateFailReasonTimeout.
//
// Any other value (including unset) is a no-op so the same binary
// can participate in both the failure and pass phases of a single
// smoke run: bootstrap with the env var set, then clear it when
// installing the "good" rollback target.
//
// This file is only compiled under `-tags failstartup` and must
// never land in a production artefact.
func maybeSimulateStartupFailure(logger *slog.Logger) {
	mode := os.Getenv("CLAWMAST_FAIL_MODE")
	switch mode {
	case "crash":
		fmt.Fprintln(os.Stderr, "[failstartup] simulating crash before READY")
		os.Exit(1)
	case "hang":
		logger.Warn("failstartup: hanging forever without READY",
			"component", "worker")
		select {}
	}
}
