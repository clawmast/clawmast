//go:build unix

package supervisor

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the test binary double as a fake worker. When
// CMFAKE_MODE is set the binary runs the requested behaviour against
// the NOTIFY_SOCKET the supervisor passed in, then exits. The
// standard `go test` entry is used otherwise.
func TestMain(m *testing.M) {
	if mode := os.Getenv("CMFAKE_MODE"); mode != "" {
		runFakeWorker(mode)
		return
	}
	os.Exit(m.Run())
}

// runFakeWorker implements the small set of worker behaviours the
// integration tests exercise. It never returns — each branch calls
// os.Exit explicitly so deferred test hooks do not fire.
func runFakeWorker(mode string) {
	sockPath := os.Getenv("NOTIFY_SOCKET")
	conn, _ := dialNotify(sockPath)
	// All paths below intentionally ignore write errors: if the
	// supervisor has already gone away the test will fail on the
	// supervisor side, which is what we want.
	switch mode {
	case "no_restart":
		os.Exit(64)
	case "rollback":
		os.Exit(65)
	case "no_ready":
		select {}
	case "crash_after_ready":
		writeFrame(conn, "READY=1\n")
		time.Sleep(50 * time.Millisecond)
		os.Exit(1)
	case "ready_then_exit_zero":
		writeFrame(conn, "READY=1\n")
		time.Sleep(50 * time.Millisecond)
		os.Exit(0)
	case "ready_heartbeat":
		runReadyHeartbeat(conn, os.Getenv("WATCHDOG_USEC"), true /*obeyTerm*/)
	case "ready_ignore_term":
		runReadyHeartbeat(conn, os.Getenv("WATCHDOG_USEC"), false /*obeyTerm*/)
	case "by_path":
		// Fake worker routes on the directory name of os.Args[0]
		// so rollback tests can place one binary in "versions/bad"
		// and another in "versions/good" and see different
		// behaviour from the same underlying test binary.
		argv0 := os.Args[0]
		switch {
		case strings.Contains(argv0, "/bad/"):
			writeFrame(conn, "READY=1\n")
			time.Sleep(30 * time.Millisecond)
			os.Exit(1)
		case strings.Contains(argv0, "/good/"):
			runReadyHeartbeat(conn, os.Getenv("WATCHDOG_USEC"), true)
		default:
			fmt.Fprintln(os.Stderr, "by_path: cannot classify argv0="+argv0)
			os.Exit(2)
		}
	default:
		fmt.Fprintln(os.Stderr, "fake worker: unknown CMFAKE_MODE="+mode)
		os.Exit(2)
	}
}

// runReadyHeartbeat sends READY=1 then sends WATCHDOG=1 every usec/3
// microseconds. When obeyTerm is true a SIGTERM triggers STOPPING=1
// and a clean exit 0; otherwise SIGTERM is swallowed so the
// supervisor has to escalate to SIGKILL.
func runReadyHeartbeat(conn *net.UnixConn, usecStr string, obeyTerm bool) {
	writeFrame(conn, "READY=1\n")
	usec, _ := strconv.ParseInt(usecStr, 10, 64)
	if usec <= 0 {
		usec = 30_000_000
	}
	interval := time.Duration(usec/3) * time.Microsecond
	if interval <= 0 {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	for {
		select {
		case <-ticker.C:
			writeFrame(conn, "WATCHDOG=1\n")
		case <-sigCh:
			if obeyTerm {
				writeFrame(conn, "STOPPING=1\n")
				os.Exit(0)
			}
			// Deliberately continue looping so the watchdog
			// stays satisfied and the supervisor's only path
			// to reap us is SIGKILL after StopGrace.
		}
	}
}

// dialNotify connects to the supervisor's datagram socket. When the
// path is empty (e.g. a test forgot to wire it) the returned conn is
// nil and writers become no-ops.
func dialNotify(path string) (*net.UnixConn, error) {
	if path == "" {
		return nil, nil
	}
	return net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
}

func writeFrame(c *net.UnixConn, frame string) {
	if c == nil {
		return
	}
	_, _ = c.Write([]byte(frame))
}

// fakeWorkerCommand returns the path to the current test binary plus
// the env var that turns it into the requested fake worker. The
// caller wires it into Config.ResolveWorker and Config.ExtraEnv.
func fakeWorkerCommand(t *testing.T, mode string) (path string, extraEnv []string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe, []string{"CMFAKE_MODE=" + mode}
}
