package agent

import (
	"testing"

	"github.com/clawmast/clawmast/internal/openclaw"
)

// TestActionFinalOutcome_TrustsCLIOverProbe pins the fix for the
// "console red / badge green" contradiction: when the CLI step itself
// reports ok, the action's terminal outcome must be ok regardless of
// what a post-action probe happens to observe. The post-probe races
// with gateway warm-up and cannot be authoritative on the action's
// success or failure.
func TestActionFinalOutcome_TrustsCLIOverProbe(t *testing.T) {
	cases := []struct {
		name    string
		cli     openclaw.StepOutcome
		want    string
	}{
		// The regression: CLI exited 0, post-probe timed out → old code
		// returned "failed". New code must return "ok".
		{"cli_ok", openclaw.OutcomeOK, "ok"},
		{"cli_failed", openclaw.OutcomeFailed, "failed"},
		{"cli_timeout", openclaw.OutcomeTimeout, "timeout"},
		// Empty shouldn't happen in practice (RunAction always emits
		// an end event) but we default to ok rather than invent a
		// failure out of a missing signal.
		{"cli_empty", openclaw.StepOutcome(""), "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := actionFinalOutcome(tc.cli)
			if got != tc.want {
				t.Fatalf("actionFinalOutcome(%q) = %q, want %q", tc.cli, got, tc.want)
			}
		})
	}
}

// TestFinalOutcome_CascadeSemantics keeps the Fix cascade's existing
// alive-based verdict intact. This is a pin, not a new assertion —
// the cascade runs its own inter-tier probes and the terminal probe
// has had time to settle, so alive==expectedAlive is the right
// signal there.
func TestFinalOutcome_CascadeSemantics(t *testing.T) {
	if got := finalOutcome(true, true); got != "ok" {
		t.Fatalf("alive matches expected: got %q, want ok", got)
	}
	if got := finalOutcome(false, true); got != "failed" {
		t.Fatalf("alive mismatches expected: got %q, want failed", got)
	}
	if got := finalOutcome(false, false); got != "ok" {
		t.Fatalf("stop-style match: got %q, want ok", got)
	}
}
