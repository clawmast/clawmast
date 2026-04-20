package openclaw

import (
	"context"
	"testing"
	"time"
)

// TestComputePhase table-drives every transition in the decision
// matrix. Ordering is load-bearing (see phase.go comment), so each
// case pins a specific rule rather than asserting coincidence.
func TestComputePhase(t *testing.T) {
	now := time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-5 * time.Second) // inside 15s grace
	stale := now.Add(-30 * time.Second) // outside 15s grace
	zeroTime := time.Time{}

	cases := []struct {
		name          string
		snap          Snapshot
		currentAction string
		lastStart     time.Time
		want          Phase
	}{
		{
			name: "pre-probe is unknown",
			snap: Snapshot{Probed: false, Alive: true, Intent: IntentRunning},
			want: PhaseUnknown,
		},
		{
			name: "cli missing beats alive",
			snap: Snapshot{Probed: true, CLIMissing: true, Alive: true},
			want: PhaseMissing,
		},
		{
			name: "cli missing beats intent",
			snap: Snapshot{Probed: true, CLIMissing: true, Intent: IntentRunning},
			want: PhaseMissing,
		},
		{
			name: "intent stopped beats alive",
			snap: Snapshot{Probed: true, Alive: true, Intent: IntentStopped},
			want: PhaseStopped,
		},
		{
			name: "alive is running",
			snap: Snapshot{Probed: true, Alive: true, Intent: IntentRunning},
			want: PhaseRunning,
		},
		{
			name:          "start action in flight is starting",
			snap:          Snapshot{Probed: true, Alive: false, Intent: IntentRunning},
			currentAction: "start",
			want:          PhaseStarting,
		},
		{
			name:          "restart action in flight is starting",
			snap:          Snapshot{Probed: true, Alive: false},
			currentAction: "restart",
			want:          PhaseStarting,
		},
		{
			name:          "fix action in flight is starting",
			snap:          Snapshot{Probed: true, Alive: false},
			currentAction: "fix",
			want:          PhaseStarting,
		},
		{
			name:          "stop action in flight is not starting",
			snap:          Snapshot{Probed: true, Alive: false, Intent: IntentStopped},
			currentAction: "stop",
			want:          PhaseStopped,
		},
		{
			name:          "doctor action in flight does not paint starting",
			snap:          Snapshot{Probed: true, Alive: false, Intent: IntentRunning},
			currentAction: "doctor",
			want:          PhaseError,
		},
		{
			name:      "inside warmup grace is starting",
			snap:      Snapshot{Probed: true, Alive: false, Intent: IntentRunning},
			lastStart: recent,
			want:      PhaseStarting,
		},
		{
			name:      "outside warmup grace is error",
			snap:      Snapshot{Probed: true, Alive: false, Intent: IntentRunning},
			lastStart: stale,
			want:      PhaseError,
		},
		{
			name:      "zero lastStart does not satisfy grace",
			snap:      Snapshot{Probed: true, Alive: false, Intent: IntentRunning},
			lastStart: zeroTime,
			want:      PhaseError,
		},
		{
			name:      "alive overrides warmup grace",
			snap:      Snapshot{Probed: true, Alive: true},
			lastStart: recent,
			want:      PhaseRunning,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputePhase(tc.snap, tc.currentAction, tc.lastStart, now)
			if got != tc.want {
				t.Fatalf("ComputePhase: want %q got %q", tc.want, got)
			}
		})
	}
}

// TestProbeNowComputesStartingAfterStamp is the integration check:
// a RunAction-equivalent stamp must show up as PhaseStarting on the
// very next probe even while the fake CLI still reports !alive.
func TestProbeNowComputesStartingAfterStamp(t *testing.T) {
	bin := writeFakeBin(t, `echo '{"ok":false}'; exit 0`)
	m := NewManager(Runner{Binary: bin}, 0, silentLogger())
	m.stampStartAttempt(time.Now().UTC())
	snap := m.ProbeNow(context.Background())
	if snap.Phase != PhaseStarting {
		t.Fatalf("post-stamp phase: want %q got %q (alive=%v intent=%q)",
			PhaseStarting, snap.Phase, snap.Alive, snap.Intent)
	}
}

// TestProbeNowStartingFallsToErrorAfterGrace verifies the grace
// window expires: a stamp older than WarmupGracePeriod must not
// keep the badge blue forever — the gateway really did fail to
// come up and the UI needs to surface that.
func TestProbeNowStartingFallsToErrorAfterGrace(t *testing.T) {
	bin := writeFakeBin(t, `echo '{"ok":false}'; exit 0`)
	m := NewManager(Runner{Binary: bin}, 0, silentLogger())
	m.SetIntent(IntentRunning)
	m.lastStartAttemptAt.Store(time.Now().Add(-2 * WarmupGracePeriod).UnixNano())
	snap := m.ProbeNow(context.Background())
	if snap.Phase != PhaseError {
		t.Fatalf("stale-stamp phase: want %q got %q", PhaseError, snap.Phase)
	}
}

// TestRunActionStampsEvenOnFailedRestart pins the behaviour we rely
// on to keep the badge blue through a real-world restart: the
// `openclaw gateway restart` CLI frequently exits non-zero from its
// own post-restart self-check (the gateway's HTTP /health has not
// warmed up yet), but RunAction must still anchor the warmup window
// so ComputePhase returns PhaseStarting until the grace expires.
// Gating the stamp on OutcomeOK regressed the fix for the "重启完成
// → 异常 → 运行中" flapping bug; this test prevents a future reader
// from re-introducing that gate.
func TestRunActionStampsEvenOnFailedRestart(t *testing.T) {
	// Fake CLI that fails the restart sub-command but answers health
	// probes with ok=false so the gateway is observably !alive after.
	bin := writeFakeBin(t, `
case "$1" in
  gateway) exit 1 ;;
  health) echo '{"ok":false}'; exit 0 ;;
  *) exit 0 ;;
esac
`)
	m := NewManager(Runner{Binary: bin}, 0, silentLogger())
	m.SetIntent(IntentRunning)

	ch := make(chan StepEvent, 8)
	before := time.Now()
	RunAction(context.Background(), m, ch, ActionRestart)
	for range ch {
	}
	stamp := m.LastStartAttempt()
	if stamp.IsZero() || stamp.Before(before) {
		t.Fatalf("stamp not updated on failed restart: got %v (before=%v)", stamp, before)
	}
	snap := m.Get()
	if snap.Phase != PhaseStarting {
		t.Fatalf("post-failed-restart phase: want %q got %q (alive=%v)",
			PhaseStarting, snap.Phase, snap.Alive)
	}
}
