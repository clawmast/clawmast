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
	"strconv"
	"strings"
	"syscall"
	"time"

	"aead.dev/minisign"

	"github.com/clawmast/clawmast/internal/agent"
	"github.com/clawmast/clawmast/internal/openclaw"
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
	maybeSimulateStartupFailure(logger)
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
	// restartCh is signalled once by the /api/updates/install handler
	// after selfupdate.Apply rotates the `current` symlink. The worker
	// exits cleanly (code 0); the supervisor demotes the
	// spontaneous-graceful-during-Running to ClassUnexpected and
	// respawns against the freshly-rotated link (supervisor-protocol.md
	// §4 edge rules). Buffered so the handler never blocks.
	restartCh := make(chan struct{}, 1)
	if httpAddr != "off" {
		pubKey, err := loadUpdatePubKey(os.Getenv("CLAWMAST_UPDATE_PUBKEY_FILE"))
		if err != nil {
			return fmt.Errorf("load update pubkey: %w", err)
		}
		stateDir := resolveStateDir(installRoot)
		tok, err := agent.LoadOrCreateToken(stateDir)
		if err != nil {
			return fmt.Errorf("bearer token: %w", err)
		}
		if tok.Generated {
			logger.Info("bearer token generated",
				"component", "agent",
				"path", tok.Path,
				"fingerprint", tok.Fingerprint(),
				"hint", "paste the full token once in the clawmast UI; it is stored in your browser")
		} else {
			logger.Info("bearer token loaded",
				"component", "agent",
				"path", tok.Path,
				"fingerprint", tok.Fingerprint())
		}
		ocMgr := openclaw.NewManager(openclaw.Runner{}, 0, logger).WithStateDir(stateDir)
		applyAutoheal(ocMgr, logger)
		go ocMgr.Start(ctx)
		channel, channelSource := agent.ResolveChannel(
			stateDir,
			os.Getenv("CLAWMAST_UPDATE_CHANNEL"),
			agent.ChannelStable,
		)
		logger.Info("update channel resolved",
			"component", "agent",
			"channel", channel,
			"source", channelSource)
		srv = agent.NewServer(agent.Config{
			Addr:        httpAddr,
			Logger:      logger,
			InstallRoot: installRoot,
			StateDir:    stateDir,
			RequestRollback: func() {
				select {
				case rollbackCh <- struct{}{}:
				default:
				}
			},
			RequestRestart: func() {
				select {
				case restartCh <- struct{}{}:
				default:
				}
			},
			UpdateBaseURL:     envOr("CLAWMAST_UPDATE_URL", ""),
			UpdateChannel:     channel,
			UpdatePubKey:      pubKey,
			OpenClaw:          ocMgr,
			Token:             tok,
			SupervisorVersion: os.Getenv("CLAWMAST_SUPERVISOR_VERSION"),
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
	case <-restartCh:
		// Clean exit (code 0) on purpose: the supervisor interprets
		// a graceful exit during Running as unexpected and respawns
		// against the freshly-rotated `current` link. No rollback
		// flag here — this is a roll-forward.
		logger.Info("restart requested via /api/updates/install",
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

// envTruthy returns true for common "on" spellings of an env var.
// Anything else — including "0", "false", "", "no" — is treated as
// off. The lowercase+trim keeps the policy forgiving of operator
// typos without needing a real config parser.
func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// applyAutoheal reads CLAWMAST_OPENCLAW_AUTOHEAL[ _THRESHOLD | _COOLDOWN ]
// from the environment and, when the feature is enabled, installs the
// policy on ocMgr with a Trigger that runs the full openclaw.Cascade.
// The feature is strictly opt-in — absent or falsey env value leaves
// Manager behaviour identical to the pre-autoheal baseline.
//
// Invalid overrides (non-numeric threshold, unparseable duration) are
// warned-about and fall through to the defaults so a typo cannot turn
// autoheal into a silent no-op.
func applyAutoheal(ocMgr *openclaw.Manager, logger *slog.Logger) {
	if !envTruthy(os.Getenv("CLAWMAST_OPENCLAW_AUTOHEAL")) {
		return
	}
	cfg := openclaw.AutohealConfig{Enabled: true}
	if v := os.Getenv("CLAWMAST_OPENCLAW_AUTOHEAL_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Threshold = n
		} else {
			logger.Warn("openclaw: ignoring invalid CLAWMAST_OPENCLAW_AUTOHEAL_THRESHOLD",
				"component", "openclaw", "value", v)
		}
	}
	if v := os.Getenv("CLAWMAST_OPENCLAW_AUTOHEAL_COOLDOWN"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.Cooldown = d
		} else {
			logger.Warn("openclaw: ignoring invalid CLAWMAST_OPENCLAW_AUTOHEAL_COOLDOWN",
				"component", "openclaw", "value", v)
		}
	}
	// Trigger drains Cascade's event channel so the goroutine does not
	// block on a full buffer. The cascade itself logs and updates the
	// snapshot via internal probes — we only care about the side effect.
	cfg.Trigger = func(ctx context.Context) {
		evCh := make(chan openclaw.StepEvent, 8)
		go openclaw.Cascade(ctx, ocMgr, evCh)
		for range evCh {
		}
	}
	// Resolve defaults here too so the boot log matches the Manager's
	// effective policy without a getter. WithAutoheal applies the same
	// defaults internally.
	threshold := cfg.Threshold
	if threshold <= 0 {
		threshold = openclaw.DefaultAutohealThreshold
	}
	cooldown := cfg.Cooldown
	if cooldown <= 0 {
		cooldown = openclaw.DefaultAutohealCooldown
	}
	ocMgr.WithAutoheal(cfg)
	logger.Info("openclaw: autoheal enabled",
		"component", "openclaw",
		"threshold", threshold,
		"cooldown", cooldown.String())
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

// resolveStateDir picks where the bearer-token file lives. When the
// worker runs under a clawmastd install root the supervisor-visible
// <root>/state directory is preferred so both sides see the same file.
// Standalone runs fall back to ~/.clawmast/state which stays per-user
// without needing write access to the install tree.
func resolveStateDir(installRoot string) string {
	if installRoot != "" {
		return filepath.Join(installRoot, "state")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".clawmast", "state")
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

// loadUpdatePubKey returns the minisign public key the agent will use
// to verify update manifests. An empty path returns the zero value,
// which agent.NewServer treats as "fall back to the embedded dev key".
// A non-empty path must point at a minisign text-format public key
// file (the same shape clawmast-release keygen produces).
//
// This hook exists so the update-channel smoke test can point the
// worker at an ephemeral keypair without rebuilding the binary.
func loadUpdatePubKey(path string) (minisign.PublicKey, error) {
	if path == "" {
		return minisign.PublicKey{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return minisign.PublicKey{}, fmt.Errorf("read %s: %w", path, err)
	}
	var pk minisign.PublicKey
	if err := pk.UnmarshalText(raw); err != nil {
		return minisign.PublicKey{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return pk, nil
}
