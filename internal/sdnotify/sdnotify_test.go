//go:build unix

package sdnotify_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/clawmast/clawmast/internal/sdnotify"
)

// shortSocketDir returns a short temp directory under /tmp. macOS
// caps sun_path at 104 bytes, and t.TempDir() under $TMPDIR
// (/var/folders/…) is close to that limit for long test names.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "cmsd")
	if err != nil {
		t.Fatalf("short temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// dialListener creates a unixgram listener in a short temp dir and
// returns its path plus a drain channel emitting every datagram it
// receives.
func dialListener(t *testing.T) (string, <-chan string) {
	t.Helper()
	dir := shortSocketDir(t)
	path := filepath.Join(dir, "notify.sock")
	addr := &net.UnixAddr{Name: path, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Fatalf("listen unixgram: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	out := make(chan string, 16)
	go func() {
		defer close(out)
		buf := make([]byte, 4096)
		for {
			n, _, err := conn.ReadFromUnix(buf)
			if err != nil {
				return
			}
			out <- string(buf[:n])
		}
	}()
	return path, out
}

func TestOpenNoSocket(t *testing.T) {
	t.Setenv(sdnotify.NotifySocketEnv, "")
	c, err := sdnotify.Open()
	if !errors.Is(err, sdnotify.ErrNotSupervised) {
		t.Fatalf("want ErrNotSupervised, got %v", err)
	}
	if c != nil {
		t.Fatalf("want nil client when unsupervised, got %v", c)
	}
}

func TestOpenDialFailure(t *testing.T) {
	t.Setenv(sdnotify.NotifySocketEnv, filepath.Join(t.TempDir(), "does-not-exist.sock"))
	_, err := sdnotify.Open()
	if err == nil || errors.Is(err, sdnotify.ErrNotSupervised) {
		t.Fatalf("want real dial error, got %v", err)
	}
}

func TestClientReadyWatchdogStopping(t *testing.T) {
	path, incoming := dialListener(t)
	t.Setenv(sdnotify.NotifySocketEnv, path)

	c, err := sdnotify.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()

	if c.Path() != path {
		t.Fatalf("Path mismatch: %q != %q", c.Path(), path)
	}

	if err := c.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if err := c.Watchdog(); err != nil {
		t.Fatalf("Watchdog: %v", err)
	}
	if err := c.Stopping(); err != nil {
		t.Fatalf("Stopping: %v", err)
	}

	want := []string{"READY=1", "WATCHDOG=1", "STOPPING=1"}
	for i, w := range want {
		select {
		case got := <-incoming:
			if got != w {
				t.Fatalf("frame %d: want %q got %q", i, w, got)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("frame %d: timeout waiting for %q", i, w)
		}
	}
}

func TestClientSendMultiKey(t *testing.T) {
	path, incoming := dialListener(t)
	t.Setenv(sdnotify.NotifySocketEnv, path)

	c, err := sdnotify.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()

	if err := c.Send("READY=1", "STATUS=warming up"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case got := <-incoming:
		if !strings.Contains(got, "READY=1") || !strings.Contains(got, "STATUS=warming up") {
			t.Fatalf("unexpected datagram: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for multi-key datagram")
	}
}

func TestClientRejectsNewline(t *testing.T) {
	path, _ := dialListener(t)
	t.Setenv(sdnotify.NotifySocketEnv, path)

	c, err := sdnotify.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()

	if err := c.Send("STATUS=line1\nREADY=1"); err == nil {
		t.Fatalf("want error for embedded newline, got nil")
	}
}

func TestCloseNil(t *testing.T) {
	var c *sdnotify.Client
	if err := c.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

func TestWatchdogInterval(t *testing.T) {
	t.Setenv(sdnotify.WatchdogUsecEnv, "")
	if _, ok := sdnotify.WatchdogInterval(); ok {
		t.Fatalf("want !ok when WATCHDOG_USEC unset")
	}

	t.Setenv(sdnotify.WatchdogUsecEnv, "bogus")
	if _, ok := sdnotify.WatchdogInterval(); ok {
		t.Fatalf("want !ok when WATCHDOG_USEC unparseable")
	}

	// 30 s in usec → one-third cadence is 10 s.
	t.Setenv(sdnotify.WatchdogUsecEnv, strconv.Itoa(30_000_000))
	got, ok := sdnotify.WatchdogInterval()
	if !ok || got != 10*time.Second {
		t.Fatalf("want 10s ok, got %v %v", got, ok)
	}

	// 1.5 s in usec → floor at 1 s.
	t.Setenv(sdnotify.WatchdogUsecEnv, strconv.Itoa(1_500_000))
	got, ok = sdnotify.WatchdogInterval()
	if !ok || got != time.Second {
		t.Fatalf("want 1s ok, got %v %v", got, ok)
	}
}

func TestWatchdogPIDMatches(t *testing.T) {
	t.Setenv(sdnotify.WatchdogPIDEnv, "")
	if !sdnotify.WatchdogPIDMatches() {
		t.Fatalf("unset WATCHDOG_PID must match")
	}
	t.Setenv(sdnotify.WatchdogPIDEnv, strconv.Itoa(os.Getpid()))
	if !sdnotify.WatchdogPIDMatches() {
		t.Fatalf("self pid must match")
	}
	t.Setenv(sdnotify.WatchdogPIDEnv, strconv.Itoa(os.Getpid()+1))
	if sdnotify.WatchdogPIDMatches() {
		t.Fatalf("foreign pid must not match")
	}
	t.Setenv(sdnotify.WatchdogPIDEnv, "not-a-pid")
	if sdnotify.WatchdogPIDMatches() {
		t.Fatalf("garbage pid must not match")
	}
}
