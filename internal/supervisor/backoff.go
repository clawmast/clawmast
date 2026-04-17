//go:build unix

package supervisor

import (
	"math/rand/v2"
	"time"
)

// Backoff defaults per architecture/supervisor-protocol.md §6.
const (
	defaultBackoffBase     = 1 * time.Second
	defaultBackoffMax      = 60 * time.Second
	defaultSteadyReset     = 5 * time.Minute
	defaultCrashWindow     = 60 * time.Second
	defaultCrashCap        = 3
	defaultJitterLowNumer  = 8 // → 0.8
	defaultJitterHighNumer = 12 // → 1.2, exclusive upper per protocol
	defaultJitterDenom     = 10
)

// BackoffPolicy encapsulates the restart timing rules. All fields have
// sensible defaults applied by NewBackoff; callers usually only tweak
// them in tests to exercise edge cases quickly.
type BackoffPolicy struct {
	Base        time.Duration
	Max         time.Duration
	SteadyReset time.Duration
	CrashWindow time.Duration
	CrashCap    int
	// Rand allows tests to inject a deterministic RNG. Nil means
	// use a fresh rand/v2 PCG seeded from time.Now.
	Rand *rand.Rand
}

// NewBackoff returns a BackoffPolicy with protocol-defined defaults.
func NewBackoff() BackoffPolicy {
	return BackoffPolicy{
		Base:        defaultBackoffBase,
		Max:         defaultBackoffMax,
		SteadyReset: defaultSteadyReset,
		CrashWindow: defaultCrashWindow,
		CrashCap:    defaultCrashCap,
	}
}

// Delay computes the next backoff delay for the given number of
// consecutive crashes using
//
//	delay = min(base * 2^n, max) * jitter, jitter ∈ [0.8, 1.2)
//
// as specified in supervisor-protocol.md §6.1. Consecutive must be
// ≥ 1 (the first retry is n=1).
func (p BackoffPolicy) Delay(consecutive int) time.Duration {
	if consecutive < 1 {
		consecutive = 1
	}
	// Exponent cap: 2^62 ≫ any max, so 2^consecutive overflows
	// int64 in ~63 crashes. Clamp the shift to keep the math safe.
	shift := consecutive - 1
	if shift > 62 {
		shift = 62
	}
	d := p.Base << shift
	if d <= 0 || d > p.Max {
		d = p.Max
	}
	return jitter(d, p.Rand)
}

// Tracker maintains the sliding crash-window counter used for
// rollback decisions (§6.2, §6.3). It is not safe for concurrent use;
// the supervisor main loop owns exactly one instance.
type Tracker struct {
	policy BackoffPolicy
	// running is the time the supervisor most recently entered
	// Running. Zero when the worker has not yet reached steady
	// state in the current spawn cycle.
	running time.Time
	// crashes holds the timestamps of recent crash events, trimmed
	// to policy.CrashWindow on each RecordCrash call.
	crashes []time.Time
}

// NewTracker returns a Tracker using the given policy.
func NewTracker(p BackoffPolicy) *Tracker { return &Tracker{policy: p} }

// MarkRunning records the moment the worker entered Running. The
// SteadyReset policy uses this to decide when the consecutive-crash
// counter should be cleared.
func (t *Tracker) MarkRunning(now time.Time) { t.running = now }

// RecordCrash appends a crash timestamp, trims the sliding window,
// and applies the steady-state reset rule: if the worker spent at
// least SteadyReset in Running before the crash, the counter is
// cleared to a single entry.
func (t *Tracker) RecordCrash(now time.Time) {
	if !t.running.IsZero() && now.Sub(t.running) >= t.policy.SteadyReset {
		t.crashes = t.crashes[:0]
	}
	t.running = time.Time{}
	t.crashes = append(t.crashes, now)
	cutoff := now.Add(-t.policy.CrashWindow)
	keep := t.crashes[:0]
	for _, ts := range t.crashes {
		if !ts.Before(cutoff) {
			keep = append(keep, ts)
		}
	}
	t.crashes = keep
}

// Consecutive returns the number of crashes currently inside the
// sliding window.
func (t *Tracker) Consecutive() int { return len(t.crashes) }

// ShouldRollback reports whether the crash-cap has been hit inside
// the sliding window.
func (t *Tracker) ShouldRollback() bool {
	return len(t.crashes) >= t.policy.CrashCap
}

// Reset clears the tracker (used after a successful rollback so the
// new current version starts with a clean budget per §6.3).
func (t *Tracker) Reset() {
	t.crashes = t.crashes[:0]
	t.running = time.Time{}
}

// jitter multiplies d by a uniform factor in [0.8, 1.2). The factor
// is chosen with integer arithmetic so the protocol-specified bounds
// are honoured exactly.
func jitter(d time.Duration, r *rand.Rand) time.Duration {
	numer := defaultJitterLowNumer + intn(r, defaultJitterHighNumer-defaultJitterLowNumer)
	return time.Duration(int64(d) * int64(numer) / int64(defaultJitterDenom))
}

func intn(r *rand.Rand, n int) int {
	if r == nil {
		return rand.IntN(n)
	}
	return r.IntN(n)
}
