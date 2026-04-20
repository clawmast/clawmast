package agent

import (
	"context"
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

// TestActionSurvivesClientDisconnect pins the contract that a client
// refreshing or closing the tab mid-action must NOT kill the CLI.
// Before the fix this test targets, defer cancel() on the handler
// would fire on return-from-clientDone, SIGKILL the CLI, and stamp
// the ring buffer with outcome=failed exit_code=-1 even though the
// operator's restart had done its useful work. A refreshed client
// would then replay "重启失败" into the console.
//
// The test forces the race by making the fake CLI sleep long enough
// that we can abort the HTTP client while the action is still running,
// then asserts the ring buffer eventually reports ok.
func TestActionSurvivesClientDisconnect(t *testing.T) {
	bin := writeFakeOpenclawBin(t, `
case "$1-$2" in
  gateway-restart) sleep 2; echo "Restarted LaunchAgent"; exit 0 ;;
  health) echo '{"ok":true,"ts":1,"sessions":{"count":0}}' ;;
  *) exit 99 ;;
esac
`)
	m := openclaw.NewManager(openclaw.Runner{Binary: bin}, time.Second, nil)
	srv := NewServer(Config{OpenClaw: m})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Short client timeout so curl-equivalent disconnects well before
	// the fake CLI (2 s sleep) finishes. 200 ms gives the server time
	// to start the CLI; we need the clientDone branch to fire while
	// RunAction is still mid-sleep.
	clientCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(clientCtx, "POST",
		ts.URL+"/api/openclaw/action?name=restart", nil)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		// Drain whatever arrived before the deadline, then abort.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	// err is expected (context deadline exceeded); we explicitly do
	// NOT fail on it — the whole point is to simulate a refresh.

	// Poll the action log until the CLI has finished. Upper bound is
	// generous (5 s) so a slow CI runner doesn't flake; the CLI itself
	// sleeps 2 s plus overhead.
	deadline := time.Now().Add(5 * time.Second)
	var entry openclaw.ActionLogEntry
	for time.Now().Before(deadline) {
		entry = m.ActionLog()
		if entry.Name == "restart" && !entry.Running {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if entry.Name != "restart" {
		t.Fatalf("action log name: got %q, want restart", entry.Name)
	}
	if entry.Running {
		t.Fatalf("action log still Running=true after %v; CLI was killed?", 5*time.Second)
	}
	if entry.Outcome != "ok" {
		t.Fatalf("action outcome: got %q, want ok (disconnect must not kill CLI)",
			entry.Outcome)
	}
	if len(entry.Events) < 2 {
		t.Fatalf("want at least start+end events, got %d", len(entry.Events))
	}
	// The end event must carry exit_code=0 (not -1 from a signal) and
	// the stdout tail the CLI actually printed.
	end := entry.Events[len(entry.Events)-1]
	if end.Phase != "end" {
		t.Fatalf("last event phase: got %q, want end", end.Phase)
	}
	if end.ExitCode != 0 {
		t.Fatalf("end.ExitCode: got %d, want 0 (non-zero means SIGKILL)", end.ExitCode)
	}
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
