package openclaw

import "time"

// shouldAutoheal returns true when every gate of the opt-in self-heal
// policy is satisfied for s at now. Kept as a pure function so the
// decision table can be exercised without standing up a Manager or a
// fake-bin Runner. Callers in the probe loop already hold the store
// lock when they pass s, so there is no race against a concurrent
// probe mutating the consecutive-down counter.
//
// The gates mirror the invariants documented on AutohealConfig:
//
//  1. cfg.Enabled must be true — autoheal is strictly opt-in.
//  2. The gateway must be observed !alive right now. A mid-flight
//     recovery should never trigger a cascade we no longer need.
//  3. Intent != IntentStopped. A deliberate operator stop is honoured
//     above crash-resilience (R2: "supervisor / worker split is
//     non-negotiable" does not license us to undo a human click).
//  4. CLIMissing must be false. Without the openclaw binary there is
//     nothing to cascade through; firing would only log noise.
//  5. The consecutive !alive streak must have reached cfg.Threshold.
//     Below-threshold ticks filter single-probe flaps.
//  6. now must be at or after s.AutohealNextEligible. A zero
//     NextEligible means "never fired", which always passes this
//     gate so the first trigger can land immediately.
func shouldAutoheal(s *Snapshot, cfg AutohealConfig, now time.Time) bool {
	switch {
	case !cfg.Enabled:
		return false
	case s.Alive:
		return false
	case s.Intent == IntentStopped:
		return false
	case s.CLIMissing:
		return false
	case s.AutohealConsecutiveDown < cfg.Threshold:
		return false
	case !s.AutohealNextEligible.IsZero() && now.Before(s.AutohealNextEligible):
		return false
	default:
		return true
	}
}
