//go:build unix

package supervisor

import (
	"testing"
	"time"
)

// baseTime is an arbitrary fixed instant the tests advance by known
// offsets. Using a fixed epoch keeps assertions on derived deadlines
// stable across CI runs.
var baseTime = time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)

func newTestGate() *HealthGate {
	return NewHealthGate(90*time.Second, 15*time.Second)
}

func TestHealthGate_ArmThenStableThenPass(t *testing.T) {
	g := newTestGate()
	armed := g.Arm("v0.0.2", baseTime, baseTime)
	if !armed || !g.IsActive() {
		t.Fatalf("expected gate to arm")
	}
	// READY at t+5s, then tick at t+20s — 15s of stable time.
	g.OnReady(baseTime.Add(5 * time.Second))
	out := g.Tick(baseTime.Add(20 * time.Second))
	if out != GateOutcomePass {
		t.Fatalf("expected Pass at stable-by, got %v", out)
	}
	if !g.Passed() || g.IsActive() {
		t.Fatalf("gate should be Passed and not Active")
	}
}

func TestHealthGate_CrashInWindowFailsImmediately(t *testing.T) {
	g := newTestGate()
	g.Arm("v0.0.2", baseTime, baseTime)
	// Crash at t+1s — well inside the window, with a single crash.
	// The gate must fail the install without waiting for CrashCap.
	out := g.OnCrash(baseTime.Add(1 * time.Second))
	if out != GateOutcomeCrashed {
		t.Fatalf("expected Crashed, got %v", out)
	}
	if g.IsActive() || g.FailReason() != GateFailReasonCrash {
		t.Fatalf("wrong state after crash: active=%v reason=%q", g.IsActive(), g.FailReason())
	}
}

func TestHealthGate_TimeoutNeverReady(t *testing.T) {
	g := newTestGate()
	g.Arm("v0.0.2", baseTime, baseTime)
	// Tick at t+91s without ever calling OnReady.
	out := g.Tick(baseTime.Add(91 * time.Second))
	if out != GateOutcomeTimedOut {
		t.Fatalf("expected TimedOut, got %v", out)
	}
	if g.FailReason() != GateFailReasonTimeout {
		t.Fatalf("wrong fail reason: %q", g.FailReason())
	}
}

func TestHealthGate_TimeoutReadyTooLate(t *testing.T) {
	g := newTestGate()
	g.Arm("v0.0.2", baseTime, baseTime)
	// READY at t+80s — we only have 10s left before deadline, but
	// stableFor is 15s, so the deadline elapses before we stabilise.
	g.OnReady(baseTime.Add(80 * time.Second))
	out := g.Tick(baseTime.Add(91 * time.Second))
	if out != GateOutcomeTimedOut {
		t.Fatalf("expected TimedOut, got %v", out)
	}
}

func TestHealthGate_PassBeforeDeadlineWhenReadyEarly(t *testing.T) {
	g := newTestGate()
	g.Arm("v0.0.2", baseTime, baseTime)
	g.OnReady(baseTime.Add(2 * time.Second))
	// Stable-by is t+17s, well before the t+90s deadline.
	// Ticking at t+10s must still be Pending.
	if out := g.Tick(baseTime.Add(10 * time.Second)); out != GateOutcomePending {
		t.Fatalf("expected Pending at t+10s, got %v", out)
	}
	if out := g.Tick(baseTime.Add(17 * time.Second)); out != GateOutcomePass {
		t.Fatalf("expected Pass at stable-by, got %v", out)
	}
}

func TestHealthGate_CrashAfterPassIsIgnored(t *testing.T) {
	g := newTestGate()
	g.Arm("v0.0.2", baseTime, baseTime)
	g.OnReady(baseTime.Add(1 * time.Second))
	if g.Tick(baseTime.Add(16*time.Second + 1)); !g.Passed() {
		t.Fatalf("gate should have passed")
	}
	// A crash after Pass must not re-fail the gate; the outer loop
	// handles post-commit crashes via the existing crash-cap path.
	if out := g.OnCrash(baseTime.Add(30 * time.Second)); out != GateOutcomePending {
		t.Fatalf("post-pass crash must return Pending, got %v", out)
	}
}

func TestHealthGate_ArmRejectsStaleInstall(t *testing.T) {
	g := newTestGate()
	// installedAt is 2 minutes ago, window is 90s → already stale.
	armed := g.Arm("v0.0.2", baseTime.Add(-2*time.Minute), baseTime)
	if armed || g.IsActive() {
		t.Fatalf("stale install must not arm the gate; armed=%v active=%v", armed, g.IsActive())
	}
}

func TestHealthGate_NextWakeRespectsEarlierOfDeadlineAndStableBy(t *testing.T) {
	g := newTestGate()
	g.Arm("v0.0.2", baseTime, baseTime)
	// While Armed the gate is waiting on the deadline: 90s away.
	if d := g.NextWake(baseTime); d != 90*time.Second {
		t.Fatalf("Armed NextWake: want 90s, got %v", d)
	}
	// After READY the stable-by (t+15s) is sooner than deadline (t+90s).
	g.OnReady(baseTime)
	if d := g.NextWake(baseTime); d != 15*time.Second {
		t.Fatalf("Stabilizing NextWake: want 15s, got %v", d)
	}
	// If READY came at t+80s, deadline (t+90s) beats stable-by (t+95s).
	g2 := newTestGate()
	g2.Arm("v0.0.2", baseTime, baseTime)
	g2.OnReady(baseTime.Add(80 * time.Second))
	if d := g2.NextWake(baseTime.Add(80 * time.Second)); d != 10*time.Second {
		t.Fatalf("late-READY NextWake: want 10s (until deadline), got %v", d)
	}
}

func TestHealthGate_ZeroDurationsFallBackToDefaults(t *testing.T) {
	g := NewHealthGate(0, 0)
	if g.window != DefaultGateWindow || g.stableFor != DefaultGateStableFor {
		t.Fatalf("zero durations must fall back: window=%v stable=%v", g.window, g.stableFor)
	}
}
