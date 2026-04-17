// Package sdnotify is the worker-side client for the minimal
// sd_notify(3) subset the ClawMast supervisor listens on.
//
// The supervisor binds the NOTIFY_SOCKET path it exports via the
// worker's environment (see architecture/supervisor-protocol.md §5).
// This package writes the three v1.0 keys the supervisor acts on:
//
//   - READY=1    — exactly once when the worker is serving.
//   - WATCHDOG=1 — periodically, at ~WATCHDOG_USEC / 3.
//   - STOPPING=1 — courtesy signal when the worker begins shutdown.
//
// The package compiles on all supported GOOSes. Actual socket I/O is
// POSIX-only; on Windows Open returns errors.ErrUnsupported so the
// worker can detect the lack of supervisor support and run standalone
// (AGENTS.md R5, client-topology.md §2).
package sdnotify

import (
	"errors"
	"os"
	"strconv"
	"time"
)

// ErrNotSupervised is returned by Open when the process is not running
// under a supervisor — i.e. NOTIFY_SOCKET is not set in the
// environment. Callers should treat this like a no-op: the worker
// simply runs without sending notifications.
var ErrNotSupervised = errors.New("sdnotify: NOTIFY_SOCKET not set")

// NotifySocketEnv is the environment variable holding the absolute
// path of the supervisor's datagram socket.
const NotifySocketEnv = "NOTIFY_SOCKET"

// WatchdogUsecEnv is the environment variable holding the watchdog
// deadline in microseconds (see supervisor-protocol.md §5.1).
const WatchdogUsecEnv = "WATCHDOG_USEC"

// WatchdogPIDEnv is the environment variable carrying the pid the
// supervisor expects notifications from. When set, the worker should
// refuse to send on behalf of a different pid.
const WatchdogPIDEnv = "WATCHDOG_PID"

// WatchdogInterval returns the worker's recommended heartbeat
// cadence: WATCHDOG_USEC / 3, rounded down to at least 1s. It returns
// (0, false) when WATCHDOG_USEC is unset or unparseable, which is the
// signal to skip heartbeats.
//
// This is platform-neutral so Windows builds can still read the env
// var even though they never dial the socket.
func WatchdogInterval() (time.Duration, bool) {
	raw := os.Getenv(WatchdogUsecEnv)
	if raw == "" {
		return 0, false
	}
	usec, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || usec <= 0 {
		return 0, false
	}
	full := time.Duration(usec) * time.Microsecond
	interval := full / 3
	if interval < time.Second {
		interval = time.Second
	}
	return interval, true
}

// WatchdogPIDMatches reports whether WATCHDOG_PID either is unset or
// matches the current process. The supervisor sets WATCHDOG_PID to the
// pid it spawned; if that pid doesn't match we're almost certainly
// wedged inside a re-exec or container and should refuse to send
// heartbeats on behalf of someone else (supervisor-protocol.md §5.1).
func WatchdogPIDMatches() bool {
	raw := os.Getenv(WatchdogPIDEnv)
	if raw == "" {
		return true
	}
	pid, err := strconv.Atoi(raw)
	if err != nil {
		return false
	}
	return pid == os.Getpid()
}
