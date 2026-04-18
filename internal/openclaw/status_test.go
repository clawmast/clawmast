package openclaw

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestProbeHealthy parses the real `openclaw health --json` shape we
// saw in the integration contract and verifies Snapshot fields fill
// correctly.
func TestProbeHealthy(t *testing.T) {
	payload := `{
      "ok": true,
      "ts": 1776493713989,
      "durationMs": 7,
      "channels": {"wechat":{},"telegram":{}},
      "heartbeatSeconds": 1800,
      "defaultAgentId": "main",
      "sessions": {"count": 2}
    }`
	bin := writeFakeBin(t, "cat <<'EOF'\n"+payload+"\nEOF")
	r := Runner{Binary: bin}
	s := probe(context.Background(), r, Snapshot{})
	if !s.Alive {
		t.Fatalf("alive=false, err=%q", s.ProbeError)
	}
	if !s.Probed {
		t.Fatalf("probed=false")
	}
	if s.DefaultAgentID != "main" {
		t.Fatalf("agent id: got %q", s.DefaultAgentID)
	}
	if s.ChannelCount != 2 {
		t.Fatalf("channels: got %d", s.ChannelCount)
	}
	if s.SessionsCount != 2 {
		t.Fatalf("sessions: got %d", s.SessionsCount)
	}
	if s.LastAliveAt == "" {
		t.Fatalf("last_alive_at must be set on success")
	}
}

// TestProbeExitNonZero treats exit 1 (gateway down but CLI present) as
// alive=false with a human-readable ProbeError.
func TestProbeExitNonZero(t *testing.T) {
	bin := writeFakeBin(t, `echo "Error: gateway unreachable" 1>&2; exit 1`)
	r := Runner{Binary: bin}
	s := probe(context.Background(), r, Snapshot{})
	if s.Alive {
		t.Fatalf("alive=true on non-zero exit")
	}
	if !strings.Contains(s.ProbeError, "exit 1") {
		t.Fatalf("probe_error: want 'exit 1', got %q", s.ProbeError)
	}
	if !strings.Contains(s.ProbeError, "gateway unreachable") {
		t.Fatalf("probe_error: should include stderr tail, got %q", s.ProbeError)
	}
}

// TestProbeBinaryMissing sets CLIMissing=true so the UI can offer
// install copy instead of a red dot.
func TestProbeBinaryMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	s := probe(context.Background(), Runner{}, Snapshot{})
	if !s.CLIMissing {
		t.Fatalf("cli_missing=false")
	}
	if s.Alive {
		t.Fatalf("alive=true when CLI missing")
	}
}

// TestProbeParseError surfaces malformed JSON as ProbeError without
// crashing the poller.
func TestProbeParseError(t *testing.T) {
	bin := writeFakeBin(t, `echo "not json at all"`)
	r := Runner{Binary: bin}
	s := probe(context.Background(), r, Snapshot{})
	if s.Alive {
		t.Fatalf("alive=true on garbage output")
	}
	if !strings.Contains(s.ProbeError, "parse health json") {
		t.Fatalf("probe_error: want parse message, got %q", s.ProbeError)
	}
}

// TestProbePreservesLastAliveAt verifies a failing probe does not
// overwrite the sticky LastAliveAt from the previous successful probe,
// so the UI can show "down for Xs".
func TestProbePreservesLastAliveAt(t *testing.T) {
	prev := Snapshot{Alive: true, LastAliveAt: "2026-04-18T00:00:00Z"}
	bin := writeFakeBin(t, `exit 2`)
	r := Runner{Binary: bin}
	s := probe(context.Background(), r, prev)
	if s.Alive {
		t.Fatalf("alive stayed true")
	}
	if s.LastAliveAt != prev.LastAliveAt {
		t.Fatalf("last_alive_at changed: prev=%s now=%s", prev.LastAliveAt, s.LastAliveAt)
	}
}

// TestProbeRespectsCallerContext verifies ProbeTimeout applies via the
// runner's timeout guard.
func TestProbeRespectsCallerContext(t *testing.T) {
	bin := writeFakeBin(t, `sleep 30`)
	r := Runner{Binary: bin}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	s := probe(ctx, r, Snapshot{})
	if time.Since(start) > 3*time.Second {
		t.Fatalf("probe did not respect ctx timeout")
	}
	if s.Alive {
		t.Fatalf("alive=true on timeout")
	}
}
