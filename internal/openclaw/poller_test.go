package openclaw

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// silentLogger returns a slog.Logger that discards all output so
// Manager construction in tests does not pollute -v output with info
// lines from the probe loop.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestProbeNowPreservesExistingIntent is the baseline: a probe with no
// concurrent SetIntent should leave Intent untouched. Regression
// protection for the initial copy-prev-into-next step in probe().
func TestProbeNowPreservesExistingIntent(t *testing.T) {
	bin := writeFakeBin(t, `echo '{"ok":true,"ts":1,"sessions":{"count":0}}'; exit 0`)
	m := NewManager(Runner{Binary: bin}, 0, silentLogger())
	m.SetIntent(IntentStopped)

	snap := m.ProbeNow(context.Background())
	if !snap.Alive {
		t.Fatalf("probe: alive=false, got %+v", snap)
	}
	if snap.Intent != IntentStopped {
		t.Fatalf("intent: want %q got %q", IntentStopped, snap.Intent)
	}
}

// TestProbeNowPreservesConcurrentSetIntent is the regression test for
// the race fix in ProbeNow: if SetIntent fires while the probe is
// blocked on the CLI (2–3s in production), the post-probe store write
// must pick up the new Intent from the store, not from the stale prev
// snapshot captured at the top of ProbeNow.
//
// Before the fix, ProbeNow read prev.Intent once, then wrote the
// entire Snapshot back including prev.Intent, clobbering anything
// SetIntent wrote in the meantime. That manifested as the badge
// flipping back to 运行中 for one tick after the operator clicked 停止,
// because the action handler called SetIntent(stopped) while a probe
// from 2.5s earlier was still flushing.
func TestProbeNowPreservesConcurrentSetIntent(t *testing.T) {
	// 200ms sleep is long enough to deterministically win the race:
	// the test goroutine has plenty of slack between probe's prev
	// read and probe's return to slot SetIntent in.
	bin := writeFakeBin(t, `sleep 0.2; echo '{"ok":true,"ts":1,"sessions":{"count":0}}'; exit 0`)
	m := NewManager(Runner{Binary: bin}, 0, silentLogger())
	// Start with IntentRunning so any "clobber with prev.Intent" bug
	// would overwrite the stopped intent we set mid-flight back to
	// running, which is exactly the user-visible misbehaviour.
	m.SetIntent(IntentRunning)

	done := make(chan Snapshot, 1)
	go func() {
		done <- m.ProbeNow(context.Background())
	}()

	// Wait until the probe has grabbed prev (well under 200ms so the
	// CLI is still blocked) before clicking 停止.
	time.Sleep(50 * time.Millisecond)
	m.SetIntent(IntentStopped)

	var snap Snapshot
	select {
	case snap = <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("ProbeNow never returned")
	}

	if snap.Intent != IntentStopped {
		t.Fatalf("intent clobbered by probe: want %q got %q (alive=%v)",
			IntentStopped, snap.Intent, snap.Alive)
	}
	// Sanity: the probe itself did still write its observed data.
	if !snap.Probed {
		t.Fatalf("snap.Probed=false; probe did not land")
	}
	if !snap.Alive {
		t.Fatalf("snap.Alive=false; fake bin should have reported ok=true")
	}
}

// TestSetIntentRoundTrip is a smoke test that Get reflects the latest
// SetIntent without a probe in between. Not strictly a race test —
// just pins the store contract.
func TestSetIntentRoundTrip(t *testing.T) {
	m := NewManager(Runner{}, 0, silentLogger())
	if got := m.Get().Intent; got != IntentUnknown {
		t.Fatalf("fresh manager intent: want %q got %q", IntentUnknown, got)
	}
	m.SetIntent(IntentRunning)
	if got := m.Get().Intent; got != IntentRunning {
		t.Fatalf("after running: want %q got %q", IntentRunning, got)
	}
	m.SetIntent(IntentStopped)
	if got := m.Get().Intent; got != IntentStopped {
		t.Fatalf("after stopped: want %q got %q", IntentStopped, got)
	}
}
