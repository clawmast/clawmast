//go:build windows

package sdnotify

import "errors"

// Client is the Windows stub. The supervisor is POSIX-only per
// AGENTS.md R5 and architecture/client-topology.md §2, so the worker
// never has a NOTIFY_SOCKET to dial on Windows. The Client type is
// preserved so callers keep a single call-site shape across GOOSes.
type Client struct{}

// Open always returns errors.ErrUnsupported on Windows. Callers
// should treat this identically to ErrNotSupervised and run
// standalone.
func Open() (*Client, error) {
	return nil, errors.ErrUnsupported
}

// Path returns an empty string.
func (c *Client) Path() string { return "" }

// Send is a no-op on Windows. It returns errors.ErrUnsupported so a
// mis-wired caller surfaces the situation loudly.
func (c *Client) Send(pairs ...string) error {
	_ = pairs
	return errors.ErrUnsupported
}

// Ready is a no-op on Windows.
func (c *Client) Ready() error { return errors.ErrUnsupported }

// Watchdog is a no-op on Windows.
func (c *Client) Watchdog() error { return errors.ErrUnsupported }

// Stopping is a no-op on Windows.
func (c *Client) Stopping() error { return errors.ErrUnsupported }

// Close is a no-op on Windows.
func (c *Client) Close() error { return nil }
