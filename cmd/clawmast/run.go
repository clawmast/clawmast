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
	"path/filepath"
	"syscall"
	"time"

	"github.com/clawmast/clawmast/internal/agent"
	"github.com/clawmast/clawmast/internal/sdnotify"
	"github.com/clawmast/clawmast/internal/version"
)

// runWorker is the worker entry point. It runs until ctx is cancelled
// (by SIGINT / SIGTERM) or until an unrecoverable error surfaces.
// Iteration 0 boots the embedded UI / JSON API (architecture/refactor.md
// §9: "UI shows version, check for updates button") in parallel with
// the supervisor handshake so clawmastd can observe a full Starting →
// Running → Stopping lifecycle.
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

	// Boot the HTTP server before sending READY so health checks land
	// on a listener that is already accepting traffic. The server runs
	// in a goroutine and is shut down from the defer chain when ctx is
	// cancelled; any startup failure is surfaced via startErr.
	httpAddr := envOr("CLAWMAST_HTTP_ADDR", agent.DefaultAddr)
	installRoot := resolveInstallRoot(logger)
	var srv *agent.Server
	httpErrCh := make(chan error, 1)
	// rollbackCh is signalled once by the /api/blacklist handler when
	// the operator flags the currently-running version as bad. Buffered
	// so the handler's goroutine never blocks on shutdown.
	rollbackCh := make(chan struct{}, 1)
	if httpAddr != "off" {
		srv = agent.NewServer(agent.Config{
			Addr:        httpAddr,
			Logger:      logger,
			InstallRoot: installRoot,
			RequestRollback: func() {
				select {
				case rollbackCh <- struct{}{}:
				default:
				}
			},
		})
		go func() { httpErrCh <- srv.Start(ctx) }()
		// Give the listener a beat to bind so logs stay ordered; the
		// bound address is only known after Start.
		time.Sleep(20 * time.Millisecond)
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
	} else {
		logger.Info("http server disabled", "component", "worker", "reason", "CLAWMAST_HTTP_ADDR=off")
	}

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

	var rollbackRequested bool
	select {
	case <-ctx.Done():
	case <-rollbackCh:
		rollbackRequested = true
		logger.Warn("rollback requested via /api/blacklist",
			"component", "worker", "version", version.Version)
	case err := <-httpErrCh:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
	}

	if supervised {
		if err := client.Stopping(); err != nil {
			logger.Warn("send STOPPING failed",
				"component", "worker",
				"err", err.Error())
		}
	}
	logger.Info("worker stopped", "component", "worker")
	if rollbackRequested {
		// Surface the rollback request as a typed exit error; main
		// translates it into os.Exit(65) so the supervisor classifies
		// the exit as ClassRollback per protocol §4.
		return &exitError{code: 65, reason: "blacklisted-current-version"}
	}
	return nil
}

// exitError asks main to os.Exit with a specific code. It is the
// worker's channel to surface the protocol's reserved codes (64 no-
// restart, 65 rollback) without calling os.Exit from deep in the
// HTTP handler stack.
type exitError struct {
	code   int
	reason string
}

func (e *exitError) Error() string {
	return fmt.Sprintf("worker exit %d (%s)", e.code, e.reason)
}

// envOr returns the value of key, or fallback when the env var is unset
// or empty. Local helper to avoid pulling in a config library for one
// variable.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// resolveInstallRoot best-effort-locates the clawmastd install tree so
// supervisor-scoped endpoints (/api/history in Iteration 1, channel /
// blacklist in later iterations) can read shared state.
//
// Resolution order:
//
//  1. CLAWMAST_INSTALL_ROOT env var — explicit, used by tests and the
//     supervisor when it wants to pin a non-default root.
//  2. Walk up from os.Executable(): the production binary lives at
//     <root>/versions/vX.Y.Z/clawmast, so <root> = exe/../.. when the
//     sibling state/ and versions/ directories exist.
//
// Returns "" when neither path produces a plausible root. The agent
// degrades gracefully (503 on supervisor-scoped endpoints) in that
// case; the UI treats an empty InstallRoot as "standalone mode".
func resolveInstallRoot(logger *slog.Logger) string {
	if v := os.Getenv("CLAWMAST_INSTALL_ROOT"); v != "" {
		return v
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err == nil {
		exe = resolved
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(exe), "..", ".."))
	// Require both state/ and versions/ so we only accept a directory
	// that actually looks like an install root.
	if st, err := os.Stat(filepath.Join(root, "state")); err != nil || !st.IsDir() {
		return ""
	}
	if st, err := os.Stat(filepath.Join(root, "versions")); err != nil || !st.IsDir() {
		return ""
	}
	logger.Info("resolved install root from executable path",
		"component", "worker", "install_root", root)
	return root
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
