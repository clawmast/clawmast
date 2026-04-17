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
	"runtime"
	"time"

	"github.com/clawmast/clawmast/internal/embed"
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
}

// DefaultAddr binds loopback-only by default. Exposing the worker on a
// non-loopback interface is a deliberate act that must be gated on
// authentication (shipped in a later iteration).
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
	s := &Server{cfg: cfg, log: cfg.Logger, mux: http.NewServeMux()}
	s.routes()
	return s
}

// routes wires the Iteration 0 endpoints. Order matters only for the
// UI fallback: /api/ is an explicit subtree so the static handler at /
// never shadows it.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/version", s.handleVersion)
	s.mux.HandleFunc("POST /api/updates/check", s.handleUpdateCheck)

	uiFS, err := fs.Sub(embed.Assets, "dist")
	if err != nil {
		s.log.Warn("embed: failed to open web/dist, UI disabled", "err", err)
		s.mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "UI assets not embedded", http.StatusNotFound)
		}))
		return
	}
	s.mux.Handle("GET /", http.FileServerFS(uiFS))
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
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.log.Info("http server listening",
		"component", "agent", "addr", ln.Addr().String())

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

// Addr returns the bound listen address, useful in tests that use
// port 0 to pick a free port.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

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
