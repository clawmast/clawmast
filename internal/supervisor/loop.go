//go:build unix

package supervisor

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/clawmast/clawmast/internal/selfupdate"
)

// state is the supervisor state-machine label used in logs. The
// outside world only observes transitions (protocol §3) so the
// label is intentionally not exposed as an API.
type state string

const (
	stateStarting   state = "Starting"
	stateRunning    state = "Running"
	stateUnhealthy  state = "Unhealthy"
	stateRestarting state = "Restarting"
	stateStopping   state = "Stopping"
)

// loop is Run's body after filesystem and listener are ready. It
// spawns the worker, waits for it to exit, applies the exit-code
// policy, and either respawns after backoff or returns.
func (s *Supervisor) loop(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		path, version, err := s.cfg.ResolveWorker()
		if err != nil {
			return err
		}
		if blocked, err := s.checkBlacklist(version); err != nil {
			return err
		} else if blocked {
			continue
		}
		s.prepareGate(version)
		outcome, err := s.runOneSpawn(ctx, path, version)
		if err != nil {
			return err
		}
		switch outcome.kind {
		case outcomeStopped:
			return nil
		case outcomeNoRestart:
			s.recordHistory(HistoryEntry{
				Event: EventStop, Version: version,
				ExitCode: IntPtr(64), Reason: "worker requested no-restart",
			})
			return &StopError{Code: 64, Reason: "worker requested no-restart"}
		case outcomeRollback:
			if err := s.attemptWorkerRequestedRollback(version); err != nil {
				return err
			}
			continue
		case outcomeInstallHealthTimeout:
			if err := s.rollbackForInstallHealth(version, GateFailReasonTimeout); err != nil {
				return err
			}
			continue
		case outcomeCrash:
			// The install-gate takes precedence over the crash-cap
			// tracker: a single crash during the observation window
			// fails the install immediately. We only fall through
			// to the crash-cap path once the gate has passed or was
			// never armed in the first place.
			if s.gate.IsActive() {
				_ = s.gate.OnCrash(time.Now())
				if err := s.rollbackForInstallHealth(version, GateFailReasonCrash); err != nil {
					return err
				}
				continue
			}
			s.tracker.RecordCrash(time.Now())
			if s.tracker.ShouldRollback() {
				if err := s.attemptRollback(version); err != nil {
					return err
				}
				continue
			}
			delay := s.cfg.Backoff.Delay(s.tracker.Consecutive())
			s.log.Info("backing off before restart",
				slog.String("state", string(stateRestarting)),
				slog.Duration("delay", delay),
				slog.Int("consecutive", s.tracker.Consecutive()))
			if !sleep(ctx, delay) {
				return nil
			}
		}
	}
}

// prepareGate reads the install-gate marker and arms the HealthGate
// iff the marker's version matches the one we are about to spawn and
// the install is not yet stale. Any mismatch or stale marker is
// cleaned up in place: letting it persist would re-arm the gate on
// an innocent future spawn, which is strictly worse than dropping
// the marker. Read / parse errors are logged but not fatal — an
// unreadable marker is treated as "no marker", which preserves the
// pre-T2-03 behaviour for the current spawn.
func (s *Supervisor) prepareGate(version string) {
	s.gate.Reset()
	marker, err := selfupdate.ReadGateMarker(s.cfg.InstallRoot)
	if err != nil {
		s.log.Warn("read gate marker failed; spawning without gate",
			slog.Any("err", err))
		return
	}
	if marker.Version == "" {
		return
	}
	if marker.Version != version {
		s.log.Info("gate marker version does not match current; discarding",
			slog.String("marker", marker.Version),
			slog.String("current", version))
		_ = selfupdate.DeleteGateMarker(s.cfg.InstallRoot)
		return
	}
	if !s.gate.Arm(version, marker.InstalledAt, time.Now()) {
		s.log.Info("gate marker is stale; discarding",
			slog.String("version", version))
		_ = selfupdate.DeleteGateMarker(s.cfg.InstallRoot)
		return
	}
	s.log.Info("install health gate armed",
		slog.String("version", version),
		slog.Duration("window", s.cfg.GateWindow),
		slog.Duration("stable_for", s.cfg.GateStableFor))
}

type outcomeKind int

const (
	outcomeCrash outcomeKind = iota + 1
	outcomeNoRestart
	outcomeRollback
	outcomeStopped
	// outcomeInstallHealthTimeout is reported when the health gate
	// deadline elapses without the newly installed worker reaching
	// GateStableFor of uninterrupted Running. The outer loop swaps
	// to previous, adds a blacklist entry, and spawns again.
	outcomeInstallHealthTimeout
)

type spawnOutcome struct {
	kind outcomeKind
	info ExitInfo
}

// runOneSpawn spawns a worker, manages its lifecycle, and returns
// the outcome the outer loop needs to act on.
func (s *Supervisor) runOneSpawn(ctx context.Context, path, version string) (spawnOutcome, error) {
	s.recordHistory(HistoryEntry{Event: EventSpawn, Version: version})
	s.log.Info("spawning worker",
		slog.String("state", string(stateStarting)),
		slog.String("version", version),
		slog.String("path", path))
	sess, err := spawn(s.cfg, path, version)
	if err != nil {
		return spawnOutcome{}, err
	}
	s.current.Store(sess)
	defer func() {
		s.current.Store(nil)
		clearPidFile(s.cfg.RunDir)
	}()
	if err := sess.WritePidFile(s.cfg.RunDir); err != nil {
		s.log.Warn("write worker.pid failed", slog.Any("err", err))
	}

	startDeadline := time.NewTimer(s.cfg.StartTimeout)
	defer startDeadline.Stop()
	watchdog := time.NewTimer(0)
	if !watchdog.Stop() {
		<-watchdog.C
	}
	// gateTimer fires at the next HealthGate decision point
	// (stable-by or deadline). We keep it stopped when the gate is
	// not active; on activation we reset it to NextWake. Using a
	// single persistent timer rather than allocating per-wake keeps
	// the select's case count stable and avoids timer-leak bugs.
	gateTimer := time.NewTimer(time.Hour)
	if !gateTimer.Stop() {
		<-gateTimer.C
	}
	defer gateTimer.Stop()
	if s.gate.IsActive() {
		gateTimer.Reset(s.gate.NextWake(time.Now()))
	}
	curState := stateStarting

	for {
		select {
		case <-ctx.Done():
			s.log.Info("shutdown requested", slog.String("state", string(stateStopping)))
			r := sess.stop(context.Background())
			s.recordHistory(HistoryEntry{
				Event: EventStop, Version: version,
				ExitCode: IntPtr(r.Info.Code),
			})
			return spawnOutcome{kind: outcomeStopped, info: r.Info}, nil

		case r := <-sess.waitCh:
			cls := ClassifyExitCode(r.Info)
			// Protocol §4 edge rule: a spontaneous zero exit
			// while we are in Running (or Starting, where we
			// never initiated shutdown) is demoted to Unexpected.
			if cls == ClassGraceful {
				cls = ClassUnexpected
			}
			return s.classifyOutcome(version, r.Info, cls), nil

		case <-startDeadline.C:
			if curState == stateStarting {
				s.log.Warn("worker failed to signal READY in time",
					slog.Duration("timeout", s.cfg.StartTimeout))
				r := sess.stop(context.Background())
				return s.classifyOutcome(version, r.Info, ClassUnexpected), nil
			}

		case <-watchdog.C:
			s.log.Warn("watchdog deadline missed",
				slog.String("state", string(stateUnhealthy)),
				slog.Duration("deadline", s.cfg.WatchdogInterval))
			r := sess.stop(context.Background())
			return s.classifyOutcome(version, r.Info, ClassUnexpected), nil

		case <-gateTimer.C:
			// The gate wakes on its own timeline. Tick returns
			// one of Pass / TimedOut / Pending; the first two
			// are terminal for the gate, the last just re-arms
			// the timer for the next decision point.
			now := time.Now()
			switch s.gate.Tick(now) {
			case GateOutcomePass:
				s.log.Info("install health gate passed",
					slog.String("version", version))
				if err := selfupdate.DeleteGateMarker(s.cfg.InstallRoot); err != nil {
					s.log.Warn("delete gate marker failed", slog.Any("err", err))
				}
				s.gate.Reset()
			case GateOutcomeTimedOut:
				s.log.Warn("install health gate timed out",
					slog.String("version", version),
					slog.Duration("window", s.cfg.GateWindow))
				r := sess.stop(context.Background())
				s.recordHistory(HistoryEntry{
					Event:    EventCrash,
					Version:  version,
					ExitCode: IntPtr(r.Info.Code),
					Reason:   string(GateFailReasonTimeout),
				})
				return spawnOutcome{kind: outcomeInstallHealthTimeout, info: r.Info}, nil
			case GateOutcomePending:
				if s.gate.IsActive() {
					gateTimer.Reset(clampWake(s.gate.NextWake(now)))
				}
			}

		case msg, ok := <-s.listener.Messages():
			if !ok {
				continue
			}
			for _, k := range msg.Kinds {
				switch k {
				case NotifyReady:
					if curState == stateStarting {
						curState = stateRunning
						s.tracker.MarkRunning(time.Now())
						if !startDeadline.Stop() {
							select {
							case <-startDeadline.C:
							default:
							}
						}
						watchdog.Reset(s.cfg.WatchdogInterval)
						s.log.Info("worker READY",
							slog.String("state", string(stateRunning)),
							slog.Int("pid", sess.Pid()))
						// Inform the gate and re-arm the timer
						// on its new (sooner) NextWake target.
						if s.gate.IsActive() {
							s.gate.OnReady(time.Now())
							if !gateTimer.Stop() {
								select {
								case <-gateTimer.C:
								default:
								}
							}
							gateTimer.Reset(clampWake(s.gate.NextWake(time.Now())))
						}
					}
				case NotifyWatchdog:
					if curState == stateRunning {
						if !watchdog.Stop() {
							select {
							case <-watchdog.C:
							default:
							}
						}
						watchdog.Reset(s.cfg.WatchdogInterval)
					}
				case NotifyStopping:
					s.log.Info("worker announced STOPPING",
						slog.Int("pid", sess.Pid()))
				}
			}
		}
	}
}

// clampWake floors a NextWake duration at a small positive value so
// time.Timer.Reset never sees zero (which fires immediately on some
// platforms, producing a busy loop) or a negative (which panics).
func clampWake(d time.Duration) time.Duration {
	const min = 10 * time.Millisecond
	if d < min {
		return min
	}
	return d
}

// classifyOutcome records a crash in the history ledger and maps the
// classification to the outer-loop outcome kind.
func (s *Supervisor) classifyOutcome(version string, info ExitInfo, cls Classification) spawnOutcome {
	entry := HistoryEntry{
		Event:    EventCrash,
		Version:  version,
		ExitCode: IntPtr(info.Code),
		Reason:   cls.String(),
	}
	if info.Signaled {
		entry.Reason = "signal-" + cls.String()
	}
	s.recordHistory(entry)
	switch cls {
	case ClassNoRestart:
		return spawnOutcome{kind: outcomeNoRestart, info: info}
	case ClassRollback:
		return spawnOutcome{kind: outcomeRollback, info: info}
	default:
		return spawnOutcome{kind: outcomeCrash, info: info}
	}
}

// installSignalHandlers forwards SIGHUP to the currently-running
// worker (protocol §7). SIGTERM and SIGINT are expected to be wired
// by the caller via signal.NotifyContext on the ctx passed to Run,
// because they collapse into ctx cancellation cleanly. Keeping the
// SIGHUP handler here scoped to Supervisor means the supervisor
// owns the "current session pid" bookkeeping it needs to forward.
func (s *Supervisor) installSignalHandlers(ctx context.Context) func() {
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-hupCh:
				if sess := s.current.Load(); sess != nil {
					_ = sess.cmd.Process.Signal(syscall.SIGHUP)
				}
			}
		}
	}()
	return func() {
		signal.Stop(hupCh)
		close(done)
	}
}

// sleep blocks for d or until ctx is cancelled. Returns false when
// ctx was cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}
