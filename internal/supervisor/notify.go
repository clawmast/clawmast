//go:build unix

package supervisor

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// NotifyKind enumerates the v1.0 notify keys the supervisor acts on
// per architecture/supervisor-protocol.md §5.3. Unknown keys are
// dropped silently (§5.4) — they do not surface as a NotifyKind.
type NotifyKind int

const (
	// NotifyReady corresponds to READY=1.
	NotifyReady NotifyKind = iota + 1
	// NotifyWatchdog corresponds to WATCHDOG=1.
	NotifyWatchdog
	// NotifyStopping corresponds to STOPPING=1.
	NotifyStopping
)

// NotifyMessage is a typed fan-in event produced by Listener. Raw
// holds the full datagram so the main loop can log unusual payloads
// for debugging without reparsing.
type NotifyMessage struct {
	Kinds []NotifyKind
	Raw   string
}

// Listener owns the unix datagram socket the worker writes to.
type Listener struct {
	conn *net.UnixConn
	path string
	ch   chan NotifyMessage
}

// ListenNotify creates a SOCK_DGRAM socket at path, removing any
// stale file from a previous run. The socket is owner-only (0600):
// it lives under ~/.clawmast/run/ which is already per-user.
func ListenNotify(path string) (*Listener, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("supervisor: remove stale notify socket: %w", err)
	}
	addr := &net.UnixAddr{Name: path, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		return nil, fmt.Errorf("supervisor: listen notify socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = conn.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("supervisor: chmod notify socket: %w", err)
	}
	return &Listener{conn: conn, path: path, ch: make(chan NotifyMessage, 16)}, nil
}

// Path returns the absolute socket path (what the worker sees as
// $NOTIFY_SOCKET).
func (l *Listener) Path() string { return l.path }

// Messages returns the channel the main loop selects on. The channel
// closes when Serve returns.
func (l *Listener) Messages() <-chan NotifyMessage { return l.ch }

// Serve reads datagrams until ctx is cancelled or the socket is
// closed. It never blocks on a slow consumer: if the channel is full
// the datagram is dropped and a synthetic message with no Kinds is
// emitted best-effort (watchdog cadence is conservative enough that
// this is safe).
func (l *Listener) Serve(ctx context.Context) error {
	defer close(l.ch)
	go func() {
		<-ctx.Done()
		_ = l.conn.SetReadDeadline(time.Now())
	}()
	buf := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			return nil
		}
		_ = l.conn.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := l.conn.ReadFromUnix(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return fmt.Errorf("supervisor: notify read: %w", err)
		}
		msg := parseNotifyFrame(string(buf[:n]))
		select {
		case l.ch <- msg:
		default:
			// Drop: the consumer is behind. Watchdog re-arm is
			// idempotent so losing one datagram is harmless.
		}
	}
}

// Close releases the socket and removes the file.
func (l *Listener) Close() error {
	err := l.conn.Close()
	_ = os.Remove(l.path)
	return err
}

// parseNotifyFrame extracts recognised keys from a single datagram.
// It tolerates CRLF endings, empty lines, and values other than "1"
// (per §5.4 unknown shapes are ignored rather than raising).
func parseNotifyFrame(frame string) NotifyMessage {
	msg := NotifyMessage{Raw: frame}
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "READY":
			if v == "1" {
				msg.Kinds = append(msg.Kinds, NotifyReady)
			}
		case "WATCHDOG":
			if v == "1" {
				msg.Kinds = append(msg.Kinds, NotifyWatchdog)
			}
		case "STOPPING":
			if v == "1" {
				msg.Kinds = append(msg.Kinds, NotifyStopping)
			}
		}
	}
	return msg
}
