//go:build unix

package sdnotify

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
)

// Client is a unix-datagram sender for the sd_notify subset defined in
// architecture/supervisor-protocol.md §5. Methods are safe for
// concurrent use.
type Client struct {
	mu   sync.Mutex
	conn *net.UnixConn
	path string
}

// Open resolves NOTIFY_SOCKET and dials it. Returns ErrNotSupervised
// when the env var is empty; callers should treat this as "run
// standalone". Any other error is a real failure (bad path, permission
// denied, etc.).
func Open() (*Client, error) {
	path := os.Getenv(NotifySocketEnv)
	if path == "" {
		return nil, ErrNotSupervised
	}
	addr := &net.UnixAddr{Name: path, Net: "unixgram"}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return nil, fmt.Errorf("sdnotify: dial %s: %w", path, err)
	}
	return &Client{conn: conn, path: path}, nil
}

// Path returns the NOTIFY_SOCKET path the client is bound to. Useful
// for logs.
func (c *Client) Path() string {
	if c == nil {
		return ""
	}
	return c.path
}

// Send writes the given KEY=VALUE pairs as a single datagram. Pairs
// must not contain embedded newlines; they are joined with '\n' per
// supervisor-protocol.md §5.2.
func (c *Client) Send(pairs ...string) error {
	if c == nil || c.conn == nil {
		return ErrNotSupervised
	}
	for _, p := range pairs {
		if strings.ContainsAny(p, "\n\r") {
			return fmt.Errorf("sdnotify: pair %q contains newline", p)
		}
	}
	payload := strings.Join(pairs, "\n")
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.conn.Write([]byte(payload)); err != nil {
		return fmt.Errorf("sdnotify: write %s: %w", c.path, err)
	}
	return nil
}

// Ready sends READY=1. The supervisor transitions
// Starting → Running on receipt.
func (c *Client) Ready() error { return c.Send("READY=1") }

// Watchdog sends WATCHDOG=1, re-arming the supervisor's watchdog
// timer.
func (c *Client) Watchdog() error { return c.Send("WATCHDOG=1") }

// Stopping sends STOPPING=1, hinting that the worker has begun its
// own shutdown. The supervisor uses this only as an advisory log
// signal; it will still send SIGTERM / SIGKILL per §7.
func (c *Client) Stopping() error { return c.Send("STOPPING=1") }

// Close releases the underlying datagram socket. It is safe to call
// Close on a nil *Client.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.conn.Close()
	c.conn = nil
	return err
}
