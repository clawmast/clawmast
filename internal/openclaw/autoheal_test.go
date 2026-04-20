package openclaw

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestShouldAutoheal table-drives the pure gating decision. Kept
// separate from the integration tests below so a regression in the
// decision matrix surfaces independently of the probe-loop wiring.
func TestShouldAutoheal(t *testing.T) {
	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	future := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		s    Snapshot
		cfg  AutohealConfig
		want bool
	}{
		{
			name: "disabled",
			cfg:  AutohealConfig{Enabled: false, Threshold: 1},
			s:    Snapshot{AutohealConsecutiveDown: 5},
			want: false,
		},
		{
			name: "alive",
			cfg:  AutohealConfig{Enabled: true, Threshold: 1},
			s:    Snapshot{Alive: true, AutohealConsecutiveDown: 5},
			want: false,
		},
		{
			name: "intent stopped",
			cfg:  AutohealConfig{Enabled: true, Threshold: 1},
			s:    Snapshot{Intent: IntentStopped, AutohealConsecutiveDown: 5},
			want: false,
		},
		{
			name: "cli missing",
			cfg:  AutohealConfig{Enabled: true, Threshold: 1},
			s:    Snapshot{CLIMissing: true, AutohealConsecutiveDown: 5},
			want: false,
		},
		{
			name: "below threshold",
			cfg:  AutohealConfig{Enabled: true, Threshold: 3},
			s:    Snapshot{AutohealConsecutiveDown: 2},
			want: false,
		},
		{
			name: "at threshold, never fired",
			cfg:  AutohealConfig{Enabled: true, Threshold: 3},
			s:    Snapshot{AutohealConsecutiveDown: 3},
			want: true,
		},
		{
			name: "in cooldown",
			cfg:  AutohealConfig{Enabled: true, Threshold: 1},
			s: Snapshot{
				AutohealConsecutiveDown: 5,
				AutohealNextEligible:    future,
			},
			want: false,
		},
		{
			name: "cooldown expired",
			cfg:  AutohealConfig{Enabled: true, Threshold: 1},
			s: Snapshot{
				AutohealConsecutiveDown: 5,
				AutohealNextEligible:    past,
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldAutoheal(&tc.s, tc.cfg, now); got != tc.want {
				t.Fatalf("shouldAutoheal: want %v got %v", tc.want, got)
			}
		})
	}
}

// TestAutohealFiresAfterThresholdIntegration walks ProbeNow through
// two consecutive !alive probes with Threshold=2 and verifies the
// Trigger fires exactly once, the counter resets, Count increments,
// and the LastAt / NextEligibleAt wire fields land.
func TestAutohealFiresAfterThresholdIntegration(t *testing.T) {
	bin := writeFakeBin(t, `echo '{"ok":false}'; exit 1`)
	triggered := make(chan struct{}, 4)
	m := NewManager(Runner{Binary: bin}, 0, silentLogger()).WithAutoheal(AutohealConfig{
		Enabled:   true,
		Threshold: 2,
		Cooldown:  time.Hour,
		Trigger:   func(_ context.Context) { triggered <- struct{}{} },
	})
	ctx := context.Background()

	m.ProbeNow(ctx)
	if got := m.Get().AutohealConsecutiveDown; got != 1 {
		t.Fatalf("after probe 1: want consecutive=1 got %d", got)
	}
	if m.Get().AutohealCount != 0 {
		t.Fatalf("after probe 1: autoheal must not have fired")
	}

	m.ProbeNow(ctx)
	select {
	case <-triggered:
	case <-time.After(1 * time.Second):
		t.Fatalf("trigger did not fire within 1s")
	}
	final := m.Get()
	if final.AutohealCount != 1 {
		t.Fatalf("count: want 1 got %d", final.AutohealCount)
	}
	if final.AutohealConsecutiveDown != 0 {
		t.Fatalf("counter not reset after fire: got %d", final.AutohealConsecutiveDown)
	}
	if final.AutohealNextEligibleAt == "" {
		t.Fatalf("NextEligibleAt empty after fire")
	}
	if final.AutohealLastAt == "" {
		t.Fatalf("LastAt empty after fire")
	}
	if !final.AutohealEnabled {
		t.Fatalf("AutohealEnabled should mirror config")
	}
}

// TestAutohealRespectsCooldownIntegration drives five !alive probes
// with Threshold=1 and Cooldown=1h. Only the first probe may fire —
// the cooldown gate must block ticks 2–5.
func TestAutohealRespectsCooldownIntegration(t *testing.T) {
	bin := writeFakeBin(t, `echo '{"ok":false}'; exit 1`)
	triggered := make(chan struct{}, 10)
	m := NewManager(Runner{Binary: bin}, 0, silentLogger()).WithAutoheal(AutohealConfig{
		Enabled:   true,
		Threshold: 1,
		Cooldown:  time.Hour,
		Trigger:   func(_ context.Context) { triggered <- struct{}{} },
	})
	ctx := context.Background()
	for range 5 {
		m.ProbeNow(ctx)
	}
	// Goroutine dispatch is async; give it a moment to drain before
	// we assert "no more fires".
	time.Sleep(50 * time.Millisecond)
	count := 0
	for {
		select {
		case <-triggered:
			count++
		default:
			if count != 1 {
				t.Fatalf("trigger fired %d times, want 1 (cooldown should block)", count)
			}
			if got := m.Get().AutohealCount; got != 1 {
				t.Fatalf("snapshot count: want 1 got %d", got)
			}
			return
		}
	}
}

// TestAutohealAliveResetsCounter uses a fake bin that reports alive
// on tick 3 to verify the !alive→alive transition zeroes the
// consecutive counter so the subsequent !alive streak has to re-reach
// Threshold before firing.
func TestAutohealAliveResetsCounter(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "n")
	bin := writeFakeBin(t, `
n=0
if [ -f `+counter+` ]; then n=$(cat `+counter+`); fi
n=$((n+1))
echo $n > `+counter+`
case $n in
  3) echo '{"ok":true,"ts":1,"sessions":{"count":0}}'; exit 0 ;;
  *) echo '{"ok":false}'; exit 1 ;;
esac
`)
	triggered := make(chan struct{}, 4)
	m := NewManager(Runner{Binary: bin}, 0, silentLogger()).WithAutoheal(AutohealConfig{
		Enabled:   true,
		Threshold: 3,
		Cooldown:  time.Hour,
		Trigger:   func(_ context.Context) { triggered <- struct{}{} },
	})
	ctx := context.Background()
	for range 5 {
		m.ProbeNow(ctx)
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case <-triggered:
		t.Fatalf("trigger must not fire: alive probe at tick 3 resets counter")
	default:
	}
	snap := m.Get()
	if snap.AutohealConsecutiveDown != 2 {
		t.Fatalf("after 2 !alive following recovery: want counter=2 got %d", snap.AutohealConsecutiveDown)
	}
	if snap.AutohealCount != 0 {
		t.Fatalf("Count should stay 0: got %d", snap.AutohealCount)
	}
}

// TestAutohealRespectsIntentStopped verifies the operator-stopped
// gate: after SetIntent(IntentStopped) even a long !alive streak must
// not fire.
func TestAutohealRespectsIntentStopped(t *testing.T) {
	bin := writeFakeBin(t, `echo '{"ok":false}'; exit 1`)
	triggered := make(chan struct{}, 10)
	m := NewManager(Runner{Binary: bin}, 0, silentLogger()).WithAutoheal(AutohealConfig{
		Enabled:   true,
		Threshold: 1,
		Cooldown:  time.Hour,
		Trigger:   func(_ context.Context) { triggered <- struct{}{} },
	})
	m.SetIntent(IntentStopped)
	ctx := context.Background()
	for range 3 {
		m.ProbeNow(ctx)
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case <-triggered:
		t.Fatalf("trigger fired despite IntentStopped")
	default:
	}
}

// TestAutohealDisabledDoesNotFire is the off-by-default guard: even
// with a sustained !alive stream the manager must not call Trigger
// when Enabled=false (the nil default when WithAutoheal is never
// called, or when the env flag is unset).
func TestAutohealDisabledDoesNotFire(t *testing.T) {
	bin := writeFakeBin(t, `echo '{"ok":false}'; exit 1`)
	triggered := make(chan struct{}, 10)
	m := NewManager(Runner{Binary: bin}, 0, silentLogger()).WithAutoheal(AutohealConfig{
		Enabled: false,
		Trigger: func(_ context.Context) { triggered <- struct{}{} },
	})
	ctx := context.Background()
	for range 5 {
		m.ProbeNow(ctx)
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case <-triggered:
		t.Fatalf("trigger fired despite Enabled=false")
	default:
	}
	if m.Get().AutohealEnabled {
		t.Fatalf("AutohealEnabled snapshot field must reflect disabled config")
	}
}

// TestAutohealSuppressedDuringAction is the race-guard: when a user
// action (start/stop/restart/doctor) is in flight, the !alive probes
// that naturally occur during the gateway's cold-start window must
// not tick the threshold toward a concurrent cascade. Sets the flag
// directly to exercise the gate without depending on the full
// RunAction pipeline.
func TestAutohealSuppressedDuringAction(t *testing.T) {
	bin := writeFakeBin(t, `echo '{"ok":false}'; exit 1`)
	triggered := make(chan struct{}, 10)
	m := NewManager(Runner{Binary: bin}, 0, silentLogger()).WithAutoheal(AutohealConfig{
		Enabled:   true,
		Threshold: 1,
		Cooldown:  time.Hour,
		Trigger:   func(_ context.Context) { triggered <- struct{}{} },
	})
	m.setCurrentAction("start")
	ctx := context.Background()
	for range 5 {
		m.ProbeNow(ctx)
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case <-triggered:
		t.Fatalf("trigger fired while currentAction was set")
	default:
	}
	if got := m.Get().AutohealConsecutiveDown; got != 0 {
		t.Fatalf("counter must stay 0 during action, got %d", got)
	}
	// Clearing the slot lets a future !alive streak fire normally.
	m.clearCurrentAction()
	for range 2 {
		m.ProbeNow(ctx)
	}
	select {
	case <-triggered:
	case <-time.After(1 * time.Second):
		t.Fatalf("trigger did not fire after flag cleared")
	}
}
