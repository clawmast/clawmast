//go:build unix

package supervisor

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// shortSocketDir returns a short temp directory suitable for AF_UNIX
// socket paths. macOS caps sun_path at 104 bytes, and the default Go
// t.TempDir() under $TMPDIR (/var/folders/…) is close to that limit
// for long test names. Using /tmp sidesteps the issue on both
// supported platforms.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "cmns")
	if err != nil {
		t.Fatalf("short temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func TestParseNotifyFrame(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		kinds []NotifyKind
	}{
		{"ready", "READY=1\n", []NotifyKind{NotifyReady}},
		{"watchdog", "WATCHDOG=1\n", []NotifyKind{NotifyWatchdog}},
		{"stopping", "STOPPING=1\n", []NotifyKind{NotifyStopping}},
		{"packed", "READY=1\nSTATUS=serving\nWATCHDOG=1\n", []NotifyKind{NotifyReady, NotifyWatchdog}},
		{"unknown-keys-dropped", "MAINPID=12345\nBUSERROR=x\n", nil},
		{"ready-wrong-value", "READY=yes\n", nil},
		{"crlf", "READY=1\r\nWATCHDOG=1\r\n", []NotifyKind{NotifyReady, NotifyWatchdog}},
		{"empty", "", nil},
		{"no-trailing-newline", "WATCHDOG=1", []NotifyKind{NotifyWatchdog}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseNotifyFrame(c.in)
			if !reflect.DeepEqual(got.Kinds, c.kinds) {
				t.Fatalf("Kinds=%v want %v (raw=%q)", got.Kinds, c.kinds, c.in)
			}
		})
	}
}

func TestListenerRoundTrip(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "n.sock")
	l, err := ListenNotify(path)
	if err != nil {
		t.Fatalf("ListenNotify: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.Serve(ctx) }()

	client, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: l.Path(), Net: "unixgram"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if _, err := client.Write([]byte("READY=1\nSTATUS=ok\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case msg := <-l.Messages():
		if len(msg.Kinds) != 1 || msg.Kinds[0] != NotifyReady {
			t.Fatalf("want [NotifyReady], got %v", msg.Kinds)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("timed out waiting for NotifyReady")
	}

	if _, err := client.Write([]byte("WATCHDOG=1\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case msg := <-l.Messages():
		if len(msg.Kinds) != 1 || msg.Kinds[0] != NotifyWatchdog {
			t.Fatalf("want [NotifyWatchdog], got %v", msg.Kinds)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("timed out waiting for NotifyWatchdog")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve returned: %v", err)
	}
}

func TestListenNotifyReplacesStaleSocket(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "s.sock")
	first, err := ListenNotify(path)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	// Simulate a crash that left the socket file behind.
	_ = first.conn.Close()

	second, err := ListenNotify(path)
	if err != nil {
		t.Fatalf("second listen with stale file: %v", err)
	}
	_ = second.Close()
}
