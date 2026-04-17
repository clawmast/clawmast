// Worker run loop. This is platform-neutral: sdnotify itself is
// conditionally compiled so the code below works the same on Linux,
// macOS, and Windows. Windows simply never enters the supervised
// branch because sdnotify.Open returns errors.ErrUnsupported there.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/clawmast/clawmast/internal/sdnotify"
	"github.com/clawmast/clawmast/internal/version"
)

// runWorker is the worker entry point. It runs until ctx is cancelled
// (by SIGINT / SIGTERM) or until an unrecoverable error surfaces.
// Iteration 0 has no HTTP server yet (T0-03 et al.); the worker just
// stands up the supervisor handshake so clawmastd can observe a full
// Starting → Running → Stopping lifecycle.
func runWorker(ctx context.Context, out io.Writer, logger *slog.Logger) error {
	client, err := sdnotify.Open()
	supervised := true
	switch {
	case err == nil:
		logger.Info("connected to supervisor",
			"component", "worker",
			"notify_socket", client.Path())
		defer client.Close()
	case errors.Is(err, sdnotify.ErrNotSupervised), errors.Is(err, errors.ErrUnsupported):
		supervised = false
		logger.Info("running unsupervised",
			"component", "worker",
			"reason", err.Error())
	default:
		return fmt.Errorf("open notify socket: %w", err)
	}

	if supervised && !sdnotify.WatchdogPIDMatches() {
		return fmt.Errorf("WATCHDOG_PID mismatch: supervisor expects a different pid")
	}

	fmt.Fprintf(out, "clawmast worker %s (iteration 0 skeleton)\n", version.Full())

	if supervised {
		if err := client.Ready(); err != nil {
			return fmt.Errorf("send READY: %w", err)
		}
		logger.Info("worker ready", "component", "worker")
	}

	if supervised {
		if interval, ok := sdnotify.WatchdogInterval(); ok {
			go heartbeat(ctx, client, interval, logger)
		}
	}

	<-ctx.Done()

	if supervised {
		if err := client.Stopping(); err != nil {
			logger.Warn("send STOPPING failed",
				"component", "worker",
				"err", err.Error())
		}
	}
	logger.Info("worker stopped", "component", "worker")
	return nil
}

// heartbeat ticks WATCHDOG=1 every interval until ctx is cancelled.
// It exits on ctx.Done() and on any send error: the supervisor will
// notice the missing watchdog and do the right thing.
func heartbeat(ctx context.Context, client *sdnotify.Client, interval time.Duration, logger *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := client.Watchdog(); err != nil {
				logger.Warn("watchdog send failed",
					"component", "worker",
					"err", err.Error())
				return
			}
		}
	}
}

// workerSignalContext returns a context cancelled on SIGINT /
// SIGTERM, the two signals the supervisor (and launchd / systemd) use
// for graceful shutdown (supervisor-protocol.md §7).
func workerSignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
