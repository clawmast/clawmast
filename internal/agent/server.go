// Package agent implements the worker-side HTTP API and serves the
// embedded UI. In Iteration 0 the surface is intentionally small: a
// version endpoint, a health endpoint, and an update-check stub so the
// embedded page can render the walking-skeleton dashboard described in
// architecture/refactor.md §9.
//
// Later iterations grow this package with PTY, gateway lifecycle,
// config, and logs endpoints per architecture/local-agent.md.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"aead.dev/minisign"

	"github.com/clawmast/clawmast/internal/embed"
	"github.com/clawmast/clawmast/internal/openclaw"
	"github.com/clawmast/clawmast/internal/updater"
)

// Config controls the worker HTTP server. All fields are optional; the
// zero value listens on DefaultAddr with slog.Default and the embedded
// UI.
type Config struct {
	// Addr is the listen address. Empty falls back to DefaultAddr.
	// Set to the literal "-" (or any address the standard library
	// rejects) to disable the server entirely; callers should
	// instead simply skip the Server construction in that case.
	Addr string
	// Logger captures request and lifecycle events. Nil falls back
	// to slog.Default.
	Logger *slog.Logger
	// InstallRoot, when set, unlocks supervisor-scoped endpoints
	// (history.json, channel file, etc.). Empty keeps the server
	// running in standalone mode with those endpoints returning
	// 503 Service Unavailable.
	InstallRoot string
	// RequestRollback is invoked by the /api/blacklist handler
	// after the operator flags the currently-running version as
	// bad. The callback must cause the worker to exit with code 65
	// so the supervisor performs the symlink swap per protocol §4.
	// Nil disables the write path (GET still works).
	RequestRollback func()
	// RequestRestart is invoked by the /api/updates/install handler
	// after a successful selfupdate.Apply. The callback must cause
	// the worker to exit cleanly (code 0) so the supervisor demotes
	// the spontaneous-graceful-during-Running to ClassUnexpected and
	// respawns against the freshly-rotated `current` symlink (see
	// supervisor-protocol.md §4 edge rules). Nil disables the
	// install path (check-only mode).
	RequestRestart func()
	// UpdateBaseURL is the channel directory that serves
	// manifest.json and manifest.json.minisig. Empty disables the
	// update-check round-trip; /api/updates/check then returns
	// source="not-configured".
	UpdateBaseURL string
	// UpdateChannel is the expected channel name (e.g. "stable" or
	// "beta"). Empty defaults to updater.DefaultChannel. Manifests
	// whose channel field differs are rejected with
	// updater.ErrChannelMismatch.
	UpdateChannel string
	// UpdatePubKey, when non-zero, overrides the embedded dev public
	// key used to verify manifest signatures. Tests set this to a
	// freshly generated key; production builds leave it zero and
	// fall back to updater.MustDevPublicKey.
	UpdatePubKey minisign.PublicKey
	// OpenClaw, when non-nil, enables the /api/openclaw/* endpoints.
	// The Manager's Start loop runs independently of the HTTP server
	// lifecycle; the Server only reads Manager.Get() and forwards
	// Cascade events.
	OpenClaw *openclaw.Manager
	// Token, when non-zero, gates /api/** (except /api/health) on a
	// Bearer token matching Token.Value. Loopback clients are exempt
	// per the I3 contract: the local dashboard stays friction-free,
	// remote clients must authenticate.
	Token Token
	// SupervisorVersion is the clawmastd build string propagated to
	// the worker via CLAWMAST_SUPERVISOR_VERSION. Empty when the
	// worker is running standalone (no supervisor); the /api/version
	// handler exposes it as supervisor_version so the UI can render a
	// "工作 / 监工" pair without an extra round-trip.
	SupervisorVersion string
	// StateDir is where the channel-preference file (and, historically,
	// the bearer token) live. Empty keeps the /api/settings/channel
	// write path locked (returns 503) while GET still succeeds with the
	// ambient channel.
	StateDir string
	// CheckInterval schedules a background signed-manifest check every
	// tick so the UI can show a "new version available" hint without
	// the operator clicking anything first. Zero (the default) disables
	// the loop entirely — used in tests and in the "standalone, no
	// supervisor" config where we don't want an orphan goroutine.
	//
	// Run.go wires this to 6h for production. The loop observes the
	// Start() context, so Shutdown cancels the next tick cleanly.
	CheckInterval time.Duration
	// FirstCheckDelay offsets the first tick from process start. Zero
	// means "wait one full CheckInterval before the first check",
	// which would starve users who open the UI right after install;
	// run.go sets this to 60s so the first check fires ~1 minute after
	// clawmastd brings the worker up. Tests use 0 or a few ms to keep
	// the fast path fast.
	FirstCheckDelay time.Duration
}

// DefaultAddr binds on loopback so the first-run experience on macOS
// never triggers the Application Firewall prompt ("allow clawmastd to
// accept incoming network connections"). The bearer-token middleware
// is still enforced for any non-loopback client so opting in to LAN
// access remains safe: operators who want a Mac mini reachable from a
// laptop export CLAWMAST_HTTP_ADDR=0.0.0.0:17080 in the launchd plist.
const DefaultAddr = "127.0.0.1:17080"

// Server owns the embedded mux and the net.Listener. It is safe to
// call Start exactly once.
type Server struct {
	cfg     Config
	mux     *http.ServeMux
	srv     *http.Server
	ln      net.Listener
	log     *slog.Logger
	started time.Time
	// updater is nil when Config.UpdateBaseURL is empty; the handler
	// uses that to short-circuit to source="not-configured".
	updater *updater.Client
	// openclaw is a convenience alias kept in sync with cfg.OpenClaw
	// so handler methods can check one field instead of chasing the
	// config pointer.
	openclaw *openclaw.Manager
	// bgCheck caches the most recent background update-check outcome
	// (see RunBackgroundChecks). Nil / zero until the first tick lands.
	// This commit only populates the cache; exposing it to the UI is a
	// follow-up so the commit boundary stays at "worker now polls".
	bgCheckMu sync.Mutex
	bgCheckAt time.Time
	bgCheck   *UpdateCheckResponse
}

// NewServer constructs a Server but does not bind the listener; call
// Start to open the socket. The embedded UI is registered at / and
// JSON endpoints under /api/.
func NewServer(cfg Config) *Server {
	if cfg.Addr == "" {
		cfg.Addr = DefaultAddr
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.UpdateChannel == "" {
		cfg.UpdateChannel = updater.DefaultChannel
	}
	s := &Server{cfg: cfg, log: cfg.Logger, mux: http.NewServeMux(), openclaw: cfg.OpenClaw}
	if cfg.UpdateBaseURL != "" {
		key := cfg.UpdatePubKey
		if key.ID() == 0 {
			key = updater.MustDevPublicKey()
		}
		s.updater = updater.New(cfg.UpdateBaseURL, cfg.UpdateChannel, key)
	}
	s.routes()
	return s
}

// routes wires the HTTP endpoints and returns the public handler
// after bearer-middleware wrapping.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/version", s.handleVersion)
	s.mux.HandleFunc("GET /api/history", s.handleHistory)
	s.mux.HandleFunc("GET /api/blacklist", s.handleBlacklistList)
	s.mux.HandleFunc("POST /api/blacklist", s.handleBlacklistAdd)
	s.mux.HandleFunc("POST /api/updates/check", s.handleUpdateCheck)
	s.mux.HandleFunc("POST /api/updates/install", s.handleUpdateInstall)
	s.mux.HandleFunc("GET /api/settings/channel", s.handleChannelGet)
	s.mux.HandleFunc("POST /api/settings/channel", s.handleChannelSet)
	s.mux.HandleFunc("GET /api/openclaw/status", s.handleOpenclawStatus)
	s.mux.HandleFunc("GET /api/openclaw/action/log", s.handleOpenclawActionLog)
	s.mux.HandleFunc("POST /api/openclaw/fix", s.handleOpenclawFix)
	s.mux.HandleFunc("POST /api/openclaw/action", s.handleOpenclawAction)

	uiHandler, err := s.buildUIHandler()
	if err != nil {
		s.log.Warn("embed: failed to open web/dist, UI disabled", "err", err)
		s.mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "UI assets not embedded", http.StatusNotFound)
		}))
		return
	}
	s.mux.Handle("GET /", uiHandler)
}

// buildUIHandler returns the http.Handler that serves the worker UI.
// By default it serves from the go:embed FS baked into the binary at
// compile time. When the operator exports CLAWMAST_DEV_DIST_DIR (for
// local UI iteration without rebuild+reinstall), we serve that
// directory directly off disk with Cache-Control: no-store so every
// browser refresh picks up the latest edits. The env var must point
// at an existing directory; anything else falls back to the embedded
// FS so production deployments can't accidentally serve broken paths
// if the variable leaks into a launchd plist.
func (s *Server) buildUIHandler() (http.Handler, error) {
	if dir := os.Getenv("CLAWMAST_DEV_DIST_DIR"); dir != "" {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			s.log.Warn("ui: dev mode — serving from disk", "dir", dir)
			fileServer := http.FileServerFS(os.DirFS(dir))
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "no-store, must-revalidate")
				fileServer.ServeHTTP(w, r)
			}), nil
		}
		s.log.Warn("ui: CLAWMAST_DEV_DIST_DIR set but unusable; falling back to embedded FS", "dir", dir)
	}
	uiFS, err := fs.Sub(embed.Assets, "dist")
	if err != nil {
		return nil, err
	}
	return http.FileServerFS(uiFS), nil
}

// publicHandler returns the mux wrapped in the bearer middleware when
// Config.Token is set. /api/health stays public so launchd / uptime
// monitors can probe without credentials.
func (s *Server) publicHandler() http.Handler {
	if s.cfg.Token.Value == "" {
		return s.mux
	}
	public := map[string]bool{"/api/health": true}
	return bearerMiddleware(s.cfg.Token, public, s.log, s.mux)
}

// Start binds the listener and runs the server until ctx is cancelled
// or Shutdown is called. It returns once the HTTP server has closed.
// The bound address is reported before serving so operators can see
// it in logs even when a port-0 address was requested.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("agent: listen %s: %w", s.cfg.Addr, err)
	}
	s.ln = ln
	s.started = time.Now()
	s.srv = &http.Server{
		Handler:           s.publicHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.log.Info("http server listening",
		"component", "agent", "addr", ln.Addr().String())

	// Launch the periodic update-check loop iff CheckInterval was
	// configured. The goroutine observes the Start() context, so the
	// same ctx cancellation that stops Serve also drains the next tick.
	if s.cfg.CheckInterval > 0 {
		go s.RunBackgroundChecks(ctx)
	}

	errCh := make(chan error, 1)
	go func() {
		err := s.srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			errCh <- nil
			return
		}
		errCh <- err
	}()
	select {
	case <-ctx.Done():
		return s.Shutdown(context.Background())
	case err := <-errCh:
		return err
	}
}

// RunBackgroundChecks polls the configured update channel on a fixed
// interval and stashes the outcome in the Server's in-memory cache.
// It blocks until ctx is cancelled and is safe to call directly from
// tests that want to exercise a tick without going through Start().
//
// Each iteration budgets 30s for the round trip — generous enough to
// tolerate a tarpitted manifest host without blowing up the next tick,
// while still bounded so a hung TLS handshake cannot freeze the loop
// indefinitely. The returned UpdateCheckResponse is the same shape the
// HTTP handler emits so a follow-up commit can expose the cache to
// the UI with no extra serialization layer.
//
// Errors are logged at the appropriate level and otherwise swallowed:
// a failed check should never take the worker down. Likewise a stale
// cache entry is preferred over clearing the cache on transient
// failures — the UI's "update available" hint then ages gracefully.
func (s *Server) RunBackgroundChecks(ctx context.Context) {
	if s.cfg.CheckInterval <= 0 {
		return
	}
	delay := s.cfg.FirstCheckDelay
	if delay <= 0 {
		delay = s.cfg.CheckInterval
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		tickCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		resp := s.runUpdateCheck(tickCtx)
		cancel()
		s.recordBackgroundCheck(resp)
		t.Reset(s.cfg.CheckInterval)
	}
}

// recordBackgroundCheck stores the tick outcome under bgCheckMu and
// emits a single log line per tick so operators grepping journalctl
// can confirm the worker is talking to its channel. The snapshot is
// value-copied so a later mutation of the slice returned by the
// updater can't race with a reader.
func (s *Server) recordBackgroundCheck(resp UpdateCheckResponse) {
	now := time.Now().UTC()
	s.bgCheckMu.Lock()
	s.bgCheckAt = now
	snap := resp
	s.bgCheck = &snap
	s.bgCheckMu.Unlock()

	switch resp.Source {
	case "signed-manifest":
		if resp.UpdateAvailable {
			s.log.Info("background update check: new version",
				"component", "agent",
				"current", resp.Current,
				"latest", resp.Latest,
				"channel", resp.Channel)
		} else {
			s.log.Debug("background update check: up to date",
				"component", "agent",
				"version", resp.Current,
				"channel", resp.Channel)
		}
	case "error":
		s.log.Warn("background update check failed",
			"component", "agent",
			"error_code", resp.ErrorCode,
			"note", resp.Note)
	case "not-configured":
		// RunBackgroundChecks shouldn't fire at all in this state
		// because Start gates the goroutine on CheckInterval > 0
		// and the operator who set CheckInterval almost certainly
		// also set UpdateBaseURL; still, log at Debug so the path
		// is observable if we ever end up here.
		s.log.Debug("background update check skipped: updater not configured",
			"component", "agent")
	}
}

// LastBackgroundCheck returns a copy of the most recent background
// update-check outcome plus its timestamp, or (nil, zero) if the loop
// has not produced a result yet (either CheckInterval==0 or the first
// tick has not fired). Callers may read concurrently; the returned
// pointer is a value copy so subsequent ticks cannot race the reader.
//
// Intended for a follow-up UI endpoint that surfaces "update available
// as of N minutes ago" without the operator clicking "check". Exposed
// now so the RunBackgroundChecks loop is observable in tests.
func (s *Server) LastBackgroundCheck() (*UpdateCheckResponse, time.Time) {
	s.bgCheckMu.Lock()
	defer s.bgCheckMu.Unlock()
	if s.bgCheck == nil {
		return nil, time.Time{}
	}
	snap := *s.bgCheck
	return &snap, s.bgCheckAt
}

// Addr returns the bound listen address, useful in tests that use
// port 0 to pick a free port.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Handler returns the wrapped public handler (with bearer middleware
// when a token is configured), intended for use in tests that want to
// exercise the full HTTP surface through httptest.NewServer without
// booting Start's goroutine. RawMux exposes the un-wrapped mux for
// tests that need to bypass auth.
func (s *Server) Handler() http.Handler { return s.publicHandler() }

// RawMux returns the underlying mux without bearer middleware. Only
// used by tests that need to bypass auth for readability; production
// code paths go through publicHandler().
func (s *Server) RawMux() http.Handler { return s.mux }

// Shutdown triggers a graceful HTTP shutdown with a short timeout so
// the worker can exit promptly on SIGTERM. It is safe to call
// multiple times.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return s.srv.Shutdown(shutdownCtx)
}

// writeJSON is a tiny helper to keep the handlers readable.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// goVersion is resolved at init time so handlers can reuse it.
var goVersion = runtime.Version()
