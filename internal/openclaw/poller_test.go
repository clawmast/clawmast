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

// TestAppendProbeSampleTrimsToBound covers the ring buffer contract:
// appending beyond ProbeHistoryLen drops the oldest entries so the
// slice never grows unbounded. The sample at index 0 after N+k
// appends must be the (k+1)-th sample we inserted.
func TestAppendProbeSampleTrimsToBound(t *testing.T) {
	var history []ProbeSample
	const extra = 5
	total := ProbeHistoryLen + extra
	for i := range total {
		history = appendProbeSample(history, ProbeSample{
			At:      "t",
			Alive:   true,
			ProbeMS: int64(i),
		})
	}
	if got := len(history); got != ProbeHistoryLen {
		t.Fatalf("history length: want %d got %d", ProbeHistoryLen, got)
	}
	// Oldest surviving entry should be the (extra)-th insertion, since
	// we dropped the first `extra` samples.
	if got := history[0].ProbeMS; got != int64(extra) {
		t.Fatalf("oldest sample: want ProbeMS=%d got %d", extra, got)
	}
	// Newest entry is always the last insertion.
	if got := history[len(history)-1].ProbeMS; got != int64(total-1) {
		t.Fatalf("newest sample: want ProbeMS=%d got %d", total-1, got)
	}
}

// TestAppendProbeSampleDoesNotAliasInput guards against a sneaky
// aliasing bug where a caller keeps a reference to the pre-append
// slice and expects it untouched. appendProbeSample allocates fresh,
// so mutating the returned slice must not leak back.
func TestAppendProbeSampleDoesNotAliasInput(t *testing.T) {
	orig := []ProbeSample{{ProbeMS: 1}, {ProbeMS: 2}}
	out := appendProbeSample(orig, ProbeSample{ProbeMS: 3})
	out[0].ProbeMS = 999
	if orig[0].ProbeMS != 1 {
		t.Fatalf("append mutated input slice: orig[0]=%d", orig[0].ProbeMS)
	}
}

// TestProbeNowCountsCrashEdge pins the alive=true→false transition
// counter. Two probes with a fake bin that flips from ok to error
// should produce exactly one crash increment; a subsequent still-down
// probe must not re-count.
func TestProbeNowCountsCrashEdge(t *testing.T) {
	// A script that reads a counter file and alternates behaviour.
	// Call 1 returns alive; calls 2+ exit non-zero (unhealthy).
	dir := t.TempDir()
	counter := dir + "/n"
	bin := writeFakeBin(t, `
n=0
if [ -f `+counter+` ]; then n=$(cat `+counter+`); fi
n=$((n+1))
echo $n > `+counter+`
if [ "$n" = "1" ]; then
  echo '{"ok":true,"ts":1,"sessions":{"count":0}}'
  exit 0
fi
echo '{"ok":false}'
exit 1
`)
	m := NewManager(Runner{Binary: bin}, 0, silentLogger())

	first := m.ProbeNow(context.Background())
	if !first.Alive {
		t.Fatalf("first probe: want alive, got %+v", first)
	}
	if first.CrashCount != 0 {
		t.Fatalf("first probe: crash_count should be 0, got %d", first.CrashCount)
	}

	second := m.ProbeNow(context.Background())
	if second.Alive {
		t.Fatalf("second probe: want !alive, got %+v", second)
	}
	if second.CrashCount != 1 {
		t.Fatalf("crash edge: want count=1 got %d", second.CrashCount)
	}

	third := m.ProbeNow(context.Background())
	if third.Alive {
		t.Fatalf("third probe: want !alive, got %+v", third)
	}
	if third.CrashCount != 1 {
		t.Fatalf("sustained-down must not re-count: want 1 got %d", third.CrashCount)
	}
}

// TestProbeNowInitialDownDoesNotCount guards the boot-time edge case:
// if the very first probe reports !alive, we were never up to crash
// from, so CrashCount must stay at 0. Without the Probed guard,
// prev.Alive default-false vs next.Alive=false would be mis-read as a
// transition (false→false, no edge), but a naive implementation that
// only compared pointers or used true as the prev default would
// miscount here.
func TestProbeNowInitialDownDoesNotCount(t *testing.T) {
	bin := writeFakeBin(t, `echo '{"ok":false}'; exit 1`)
	m := NewManager(Runner{Binary: bin}, 0, silentLogger())
	snap := m.ProbeNow(context.Background())
	if snap.Alive {
		t.Fatalf("want !alive on initial down probe")
	}
	if snap.CrashCount != 0 {
		t.Fatalf("initial down must not count as crash: got %d", snap.CrashCount)
	}
}

// TestProbeNowAppendsHistory verifies every ProbeNow lands one sample
// in ProbeHistory, newest-last, and the alive bit reflects that probe.
func TestProbeNowAppendsHistory(t *testing.T) {
	bin := writeFakeBin(t, `echo '{"ok":true,"ts":1,"sessions":{"count":0}}'; exit 0`)
	m := NewManager(Runner{Binary: bin}, 0, silentLogger())
	for range 3 {
		m.ProbeNow(context.Background())
	}
	snap := m.Get()
	if got := len(snap.ProbeHistory); got != 3 {
		t.Fatalf("history len: want 3 got %d", got)
	}
	for i, s := range snap.ProbeHistory {
		if !s.Alive {
			t.Fatalf("sample %d: want alive, got %+v", i, s)
		}
	}
}
