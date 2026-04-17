//go:build unix

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
)

// StopError wraps a non-zero supervisor exit code surfaced back to
// the OS init system (launchd / systemd) per
// architecture/supervisor-protocol.md §4 and §6.3. Callers use
// errors.As to extract the numeric code.
type StopError struct {
	Code   int
	Reason string
}

// Error implements error.
func (e *StopError) Error() string {
	return fmt.Sprintf("supervisor: exiting with code %d (%s)", e.Code, e.Reason)
}

// Supervisor runs the protocol state machine for exactly one worker.
type Supervisor struct {
	cfg      Config
	log      *slog.Logger
	history  *History
	listener *Listener
	tracker  *Tracker
	current  atomic.Pointer[session]
}

// New constructs a Supervisor from cfg, applying protocol defaults.
// It does not touch the filesystem; that happens inside Run.
func New(cfg Config) (*Supervisor, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Supervisor{
		cfg:     cfg,
		log:     cfg.Logger.With("component", "supervisor"),
		tracker: NewTracker(cfg.Backoff),
	}, nil
}

// Run drives the state machine until ctx is cancelled, the worker
// requests no-restart / rollback-to-nothing, or an unrecoverable
// error surfaces. It returns nil on a graceful shutdown initiated by
// ctx cancellation.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := s.prepareFilesystem(); err != nil {
		return err
	}
	hist, err := OpenHistory(s.cfg.HistoryPath)
	if err != nil {
		return err
	}
	s.history = hist

	listener, err := ListenNotify(s.cfg.NotifySocketPath)
	if err != nil {
		return err
	}
	s.listener = listener
	defer func() { _ = listener.Close() }()

	lctx, cancelListener := context.WithCancel(context.Background())
	defer cancelListener()
	listenerErrCh := make(chan error, 1)
	go func() { listenerErrCh <- listener.Serve(lctx) }()

	cancelSignals := s.installSignalHandlers(ctx)
	defer cancelSignals()

	s.log.Info("supervisor ready",
		slog.String("notify_socket", listener.Path()),
		slog.String("history", s.cfg.HistoryPath))

	runErr := s.loop(ctx)
	cancelListener()
	<-listenerErrCh
	return runErr
}

// prepareFilesystem creates the directories the supervisor writes
// to and seeds supervisor.pid before any worker is spawned. Missing
// parents are created with 0700 so the notify socket — which
// Listener also chmods to 0600 — is reachable only to this user.
func (s *Supervisor) prepareFilesystem() error {
	dirs := []string{
		s.cfg.RunDir,
		filepath.Dir(s.cfg.HistoryPath),
		filepath.Dir(s.cfg.NotifySocketPath),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("supervisor: mkdir %s: %w", d, err)
		}
	}
	pidPath := filepath.Join(s.cfg.RunDir, "supervisor.pid")
	pid := []byte(strconv.Itoa(os.Getpid()) + "\n")
	if err := os.WriteFile(pidPath, pid, 0o600); err != nil {
		return fmt.Errorf("supervisor: write supervisor.pid: %w", err)
	}
	return nil
}

// recordHistory appends a best-effort history entry. A failure here
// must never block the state machine — the ledger is advisory, not
// load-bearing.
func (s *Supervisor) recordHistory(e HistoryEntry) {
	if err := s.history.Append(e); err != nil {
		s.log.Warn("history append failed",
			slog.String("event", string(e.Event)),
			slog.Any("err", err))
	}
}

// attemptRollback tries to swap current/previous. On success it
// resets the crash tracker and records a rollback event. When no
// rollback target is available the function returns a StopError with
// code 65 so Run surfaces the correct OS-visible exit.
func (s *Supervisor) attemptRollback(failingVersion string) error {
	err := SwapCurrentPrevious(s.cfg.InstallRoot)
	if err == nil {
		s.tracker.Reset()
		s.recordHistory(HistoryEntry{
			Event:   EventRollback,
			Version: failingVersion,
			Reason:  "crash-loop",
		})
		s.log.Warn("rolled back after crash-loop", slog.String("bad_version", failingVersion))
		return nil
	}
	if !errors.Is(err, ErrNoRollbackTarget) {
		return fmt.Errorf("supervisor: rollback failed: %w", err)
	}
	s.recordHistory(HistoryEntry{
		Event:   EventStop,
		Version: failingVersion,
		Reason:  "crash-loop-no-rollback",
	})
	return &StopError{Code: 65, Reason: "crash-loop with no rollback target"}
}
