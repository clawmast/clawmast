// Command crash-after-ready is a deliberately broken stand-in for the
// real ClawMast worker, used by scripts/rollback-smoke.sh to exercise
// the supervisor's crash-loop → rollback path
// (architecture/supervisor-protocol.md §6).
//
// It opens the supervisor's NOTIFY_SOCKET, sends READY=1 so the
// supervisor transitions Starting → Running (otherwise the
// supervisor's start-timeout would fire instead of the crash-loop
// detector), waits the configured delay, and exits with the
// configured non-zero status. The supervisor classifies that as a
// crash, increments its sliding-window counter, and after CrashCap
// crashes within CrashWindow it swaps current ↔ previous.
//
// Behaviour is tunable via env vars so the same binary can be used
// to simulate "crashes immediately", "crashes after the watchdog
// fires", or "exits with the don't-restart code 64". Defaults are
// chosen for the rollback smoke test.
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/clawmast/clawmast/internal/sdnotify"
)

func main() {
	delayMS, _ := strconv.Atoi(envOr("CMBAD_DELAY_MS", "50"))
	exitCode, _ := strconv.Atoi(envOr("CMBAD_EXIT", "1"))
	skipReady := os.Getenv("CMBAD_SKIP_READY") == "1"

	if !skipReady {
		c, err := sdnotify.Open()
		if err != nil {
			fmt.Fprintf(os.Stderr, "crash-after-ready: open notify: %v\n", err)
			os.Exit(2)
		}
		defer c.Close()
		if err := c.Ready(); err != nil {
			fmt.Fprintf(os.Stderr, "crash-after-ready: send READY: %v\n", err)
			os.Exit(2)
		}
	}

	if delayMS > 0 {
		time.Sleep(time.Duration(delayMS) * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "crash-after-ready: exiting with code %d (designed)\n", exitCode)
	os.Exit(exitCode)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
