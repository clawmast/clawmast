package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/clawmast/clawmast/internal/openclaw"
)

// writeFakeOpenclawBin writes a shell script that mimics the openclaw
// CLI so tests can exercise the action handler without a real install.
// Dispatch is on the first arg just like the real CLI:
//
//	gateway-restart -> sleeps, prints success, exits 0
//	gateway-start   -> prints success, exits 0
//	health          -> alive JSON
//
// Skips on windows (worker-only tier; the POSIX-shell fake won't run).
func writeFakeOpenclawBin(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-bin tests require a POSIX shell")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "fake-openclaw")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}
	return p
}

// TestActionLogEndpointShape verifies the /api/openclaw/action/log
// response schema matches what the frontend replay path expects. Tests
// both the empty-state (no action ever ran) and the post-action shape.
func TestActionLogEndpointShape(t *testing.T) {
	bin := writeFakeOpenclawBin(t, `
case "$1-$2" in
  gateway-start) echo "started"; exit 0 ;;
  health) echo '{"ok":true,"ts":1,"sessions":{"count":0}}' ;;
  *) exit 99 ;;
esac
`)
	m := openclaw.NewManager(openclaw.Runner{Binary: bin}, time.Second, nil)
	srv := NewServer(Config{OpenClaw: m})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Empty state: no action has run yet.
	resp, err := http.Get(ts.URL + "/api/openclaw/action/log")
	if err != nil {
		t.Fatalf("get empty log: %v", err)
	}
	var empty openclaw.ActionLogEntry
	if err := json.NewDecoder(resp.Body).Decode(&empty); err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	_ = resp.Body.Close()
	if empty.Name != "" || empty.Running || len(empty.Events) != 0 {
		t.Fatalf("empty-state mismatch: %+v", empty)
	}

	// After an action: name/events/outcome populated, running false.
	req, _ := http.NewRequest("POST", ts.URL+"/api/openclaw/action?name=start", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("start action: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	resp, _ = http.Get(ts.URL + "/api/openclaw/action/log")
	var after openclaw.ActionLogEntry
	_ = json.NewDecoder(resp.Body).Decode(&after)
	_ = resp.Body.Close()
	if after.Name != "start" || after.Running || after.Outcome != "ok" {
		t.Fatalf("post-action mismatch: %+v", after)
	}
}

// TestActionFinalOutcome_TrustsCLIOverProbe pins the fix for the
// "console red / badge green" contradiction: when the CLI step itself
// reports ok, the action's terminal outcome must be ok regardless of
// what a post-action probe happens to observe. The post-probe races
// with gateway warm-up and cannot be authoritative on the action's
// success or failure.
func TestActionFinalOutcome_TrustsCLIOverProbe(t *testing.T) {
	cases := []struct {
		name string
		cli  openclaw.StepOutcome
		want string
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
