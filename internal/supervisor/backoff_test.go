//go:build unix

package supervisor

import (
	"math/rand/v2"
	"testing"
	"time"
)

func TestDelayJitterBounds(t *testing.T) {
	p := NewBackoff()
	// Try each n from 1..6 and verify the result sits inside the
	// protocol bounds [0.8*expected, 1.2*expected).
	for n := 1; n <= 6; n++ {
		expected := p.Base << (n - 1)
		if expected > p.Max {
			expected = p.Max
		}
		lo := time.Duration(int64(expected) * 8 / 10)
		hi := time.Duration(int64(expected) * 12 / 10)
		for i := 0; i < 200; i++ {
			got := p.Delay(n)
			if got < lo || got >= hi {
				t.Fatalf("n=%d got %v outside [%v, %v)", n, got, lo, hi)
			}
		}
	}
}

func TestDelayClampsAtMax(t *testing.T) {
	p := NewBackoff()
	lo := time.Duration(int64(p.Max) * 8 / 10)
	hi := time.Duration(int64(p.Max) * 12 / 10)
	for n := 10; n <= 80; n += 10 {
		got := p.Delay(n)
		if got < lo || got >= hi {
			t.Fatalf("n=%d got %v outside [%v, %v)", n, got, lo, hi)
		}
	}
}

func TestDelayDeterministicWithSeededRand(t *testing.T) {
	p := NewBackoff()
	p.Rand = rand.New(rand.NewPCG(1, 2))
	first := p.Delay(3)
	p.Rand = rand.New(rand.NewPCG(1, 2))
	second := p.Delay(3)
	if first != second {
		t.Fatalf("expected deterministic output, got %v then %v", first, second)
	}
}

func TestTrackerCrashWindow(t *testing.T) {
	p := NewBackoff()
	p.CrashWindow = 60 * time.Second
	p.CrashCap = 3
	tr := NewTracker(p)

	t0 := time.Unix(1_000_000, 0)
	tr.RecordCrash(t0)
	tr.RecordCrash(t0.Add(10 * time.Second))
	tr.RecordCrash(t0.Add(59 * time.Second))
	if !tr.ShouldRollback() {
		t.Fatalf("expected rollback at 3 crashes within window, consecutive=%d", tr.Consecutive())
	}

	// A crash past the window pushes the earliest out.
	tr2 := NewTracker(p)
	tr2.RecordCrash(t0)
	tr2.RecordCrash(t0.Add(30 * time.Second))
	tr2.RecordCrash(t0.Add(120 * time.Second))
	if tr2.ShouldRollback() {
		t.Fatalf("old crash should have aged out, consecutive=%d", tr2.Consecutive())
	}
	if tr2.Consecutive() != 1 {
		t.Fatalf("want 1 in-window crash, got %d", tr2.Consecutive())
	}
}

func TestTrackerSteadyReset(t *testing.T) {
	p := NewBackoff()
	p.SteadyReset = 5 * time.Minute
	tr := NewTracker(p)

	t0 := time.Unix(2_000_000, 0)
	tr.RecordCrash(t0)
	tr.RecordCrash(t0.Add(5 * time.Second))
	// Worker recovers, stays up for 6 minutes, then crashes again.
	tr.MarkRunning(t0.Add(10 * time.Second))
	tr.RecordCrash(t0.Add(10*time.Second + 6*time.Minute))
	if got := tr.Consecutive(); got != 1 {
		t.Fatalf("steady-reset should leave only the new crash, got %d", got)
	}
}

func TestTrackerReset(t *testing.T) {
	p := NewBackoff()
	tr := NewTracker(p)
	tr.RecordCrash(time.Now())
	tr.RecordCrash(time.Now())
	tr.Reset()
	if tr.Consecutive() != 0 {
		t.Fatalf("Reset should clear the window, got %d", tr.Consecutive())
	}
}
