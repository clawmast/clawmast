//go:build unix

package supervisor

import "time"

// Health-gate defaults per refactor.md §?.4 (health-gated commit).
// A freshly installed version must reach Running within GateWindow
// of the install timestamp AND stay there for GateStableFor before
// the gate retires. Failing either invariant triggers an automatic
// rollback + blacklist entry so the operator can ship confidently
// without babysitting the first few minutes of every release.
const (
	DefaultGateWindow    = 90 * time.Second
	DefaultGateStableFor = 15 * time.Second
)

// gateState is the health-gate state label. The gate is a second
// state machine running alongside the Starting→Running→Unhealthy
// machine for a limited window after install; it only exists to
// make the "new version must prove itself" rule explicit.
type gateState int

const (
	gateInactive    gateState = iota // no install marker, or already passed/failed and reset
	gateArmed                        // install marker present, waiting for READY
	gateStabilizing                  // READY received, waiting for StableFor to elapse
	gatePassed                       // stable for StableFor; gate retired
	gateFailed                       // crashed or deadline missed; rollback pending
)

// GateOutcome is the decision that HealthGate.Tick / OnCrash returns
// to the supervisor loop. It is distinct from Classification: the
// gate is concerned with "was the new install healthy enough to
// commit to" and has no view on exit codes beyond "did a crash
// happen at all".
type GateOutcome int

const (
	// GateOutcomePending means the gate is still watching; the
	// caller should continue driving spawn/watchdog timers.
	GateOutcomePending GateOutcome = iota
	// GateOutcomePass means the new version has been observed
	// stable for StableFor. The gate self-retires; the caller
	// must delete the install marker.
	GateOutcomePass
	// GateOutcomeCrashed means the worker crashed while the gate
	// was still active. The caller must roll back + blacklist
	// with reason GateFailReasonCrash.
	GateOutcomeCrashed
	// GateOutcomeTimedOut means the deadline elapsed before the
	// worker stabilised (either never reached READY, or reached
	// READY too late to accumulate StableFor). The caller must
	// roll back + blacklist with reason GateFailReasonTimeout.
	GateOutcomeTimedOut
)

// GateFailReason is the string written into blacklist.json's reason
// field and the history ledger's reason field when the gate rejects
// an install. Keeping the strings exported (as constants) means the
// smoke test can assert on them without coupling to the loop's log
// output.
type GateFailReason string

const (
	GateFailReasonCrash   GateFailReason = "install-health-crash"
	GateFailReasonTimeout GateFailReason = "install-health-timeout"
)

// HealthGate is the per-install observation window. It is not safe
// for concurrent use; the supervisor main loop owns exactly one
// instance and drives it from a single goroutine.
type HealthGate struct {
	window    time.Duration
	stableFor time.Duration

	state      gateState
	version    string
	deadline   time.Time
	readyAt    time.Time
	failReason GateFailReason
}

// NewHealthGate returns a HealthGate with the given window / stable
// durations. Zero durations fall back to the protocol defaults.
func NewHealthGate(window, stableFor time.Duration) *HealthGate {
	if window <= 0 {
		window = DefaultGateWindow
	}
	if stableFor <= 0 {
		stableFor = DefaultGateStableFor
	}
	return &HealthGate{window: window, stableFor: stableFor}
}

// Arm starts watching version with deadline computed from
// installedAt + window. Callers must check Arm's return before
// treating the gate as active: if installedAt is already stale
// (installedAt + window < now) the gate is left inactive and the
// caller should delete the install marker without attempting a
// rollback.
func (g *HealthGate) Arm(version string, installedAt, now time.Time) (armed bool) {
	deadline := installedAt.Add(g.window)
	if !now.Before(deadline) {
		g.state = gateInactive
		return false
	}
	g.state = gateArmed
	g.version = version
	g.deadline = deadline
	g.readyAt = time.Time{}
	g.failReason = ""
	return true
}

// OnReady transitions the gate from Armed to Stabilizing. A second
// READY while already Stabilizing is ignored (the worker may re-send
// READY under restart scenarios we do not model here).
func (g *HealthGate) OnReady(now time.Time) {
	if g.state != gateArmed {
		return
	}
	g.state = gateStabilizing
	g.readyAt = now
}

// OnCrash reports a worker crash to the gate. Returns Crashed if the
// gate was still watching; Pending (with no state change) otherwise.
// Per the install-health contract, a single crash during the window
// fails the install — we do not wait for CrashCap — because a new
// version that falls over once is already worse than the previous
// version that was serving traffic.
func (g *HealthGate) OnCrash(now time.Time) GateOutcome {
	if g.state != gateArmed && g.state != gateStabilizing {
		return GateOutcomePending
	}
	g.state = gateFailed
	g.failReason = GateFailReasonCrash
	return GateOutcomeCrashed
}

// Tick advances the gate against the clock. Call it whenever the
// loop wakes (on NextWake, on timer, on any state change). Returns
// Pass once the worker has been Running for StableFor, or TimedOut
// once the deadline passes without reaching stable.
func (g *HealthGate) Tick(now time.Time) GateOutcome {
	switch g.state {
	case gateStabilizing:
		if now.Sub(g.readyAt) >= g.stableFor {
			g.state = gatePassed
			return GateOutcomePass
		}
		if !now.Before(g.deadline) {
			g.state = gateFailed
			g.failReason = GateFailReasonTimeout
			return GateOutcomeTimedOut
		}
	case gateArmed:
		if !now.Before(g.deadline) {
			g.state = gateFailed
			g.failReason = GateFailReasonTimeout
			return GateOutcomeTimedOut
		}
	}
	return GateOutcomePending
}

// NextWake returns the duration until the gate's next decision
// point (deadline or stable-by), whichever is sooner. When the gate
// is not active it returns 0; callers must guard on IsActive first.
func (g *HealthGate) NextWake(now time.Time) time.Duration {
	switch g.state {
	case gateArmed:
		return untilNonNegative(g.deadline, now)
	case gateStabilizing:
		stableBy := g.readyAt.Add(g.stableFor)
		if stableBy.Before(g.deadline) {
			return untilNonNegative(stableBy, now)
		}
		return untilNonNegative(g.deadline, now)
	}
	return 0
}

// IsActive reports whether the gate is still watching. The loop
// uses this to decide whether to route a spawn-level crash through
// the gate (install-rollback) or through the crash-cap tracker
// (normal crash-loop rollback).
func (g *HealthGate) IsActive() bool {
	return g.state == gateArmed || g.state == gateStabilizing
}

// Passed reports whether the last armed cycle retired cleanly.
func (g *HealthGate) Passed() bool { return g.state == gatePassed }

// Version returns the version the gate is currently (or was last)
// watching. Empty before the first Arm.
func (g *HealthGate) Version() string { return g.version }

// FailReason returns the failure reason set by the most recent
// Failed transition, or "" if the gate has not failed.
func (g *HealthGate) FailReason() GateFailReason { return g.failReason }

// Reset forces the gate back to Inactive without touching version
// or failReason. The loop calls Reset after consuming a Pass / Fail
// decision so the next spawn starts with a clean gate.
func (g *HealthGate) Reset() { g.state = gateInactive }

// untilNonNegative returns deadline-now clamped at zero so the
// supervisor loop can feed it straight into time.NewTimer without
// a negative-duration panic.
func untilNonNegative(deadline, now time.Time) time.Duration {
	d := deadline.Sub(now)
	if d < 0 {
		return 0
	}
	return d
}
