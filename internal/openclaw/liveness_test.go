package openclaw

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// withFastEndpoint points the package-level probe URL at srv.URL and
// re-enables the fast path for the duration of the test. TestMain
// disables the fast path globally so CLI-driven tests are stable; each
// HTTP-path test opts back in here and restores both knobs on cleanup.
func withFastEndpoint(t *testing.T, url string) {
	t.Helper()
	prevURL := probeFastEndpoint
	prevDisabled := probeFastDisabled
	probeFastEndpoint = url
	probeFastDisabled = false
	t.Cleanup(func() {
		probeFastEndpoint = prevURL
		probeFastDisabled = prevDisabled
	})
}

// TestProbeFastPathSkipsCLI wires probe() to a fake /health that returns
// ok=true, ensures Alive flips without spawning the CLI, and verifies
// LastProbeMS reflects the HTTP duration rather than the CLI timeout.
func TestProbeFastPathSkipsCLI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ok":true,"status":"live"}`)
	}))
	t.Cleanup(srv.Close)
	withFastEndpoint(t, srv.URL)

	// Runner has no Binary, so a CLI spawn would fail with ErrBinaryMissing.
	// The fast path must succeed before we ever try.
	prev := Snapshot{
		Alive:          true,
		LastEnrichedAt: rfc3339(time.Now().UTC()),
		SessionsCount:  7, // sentinel: should survive the throttled tick
	}
	s := probe(context.Background(), Runner{}, prev)
	if !s.Alive {
		t.Fatalf("alive=false, err=%q", s.ProbeError)
	}
	if s.SessionsCount != 7 {
		t.Fatalf("enrichment ran unexpectedly; sessions=%d", s.SessionsCount)
	}
	if s.LastProbeMS > int64(LivenessTimeout/time.Millisecond) {
		t.Fatalf("probe_ms=%d exceeds liveness budget", s.LastProbeMS)
	}
}

// TestProbeFastPathTriggersEnrichmentOnTransition verifies that when
// the gateway just came back alive (prev.Alive=false), the CLI runs
// even though liveness succeeded — so channel / session counts refresh
// within one tick of recovery.
func TestProbeFastPathTriggersEnrichmentOnTransition(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	withFastEndpoint(t, srv.URL)

	bin := writeFakeBin(t, `cat <<'EOF'
{"ok":true,"ts":2,"sessions":{"count":3},"channels":{"a":{},"b":{}},"defaultAgentId":"main"}
EOF`)
	r := Runner{Binary: bin}

	prev := Snapshot{Alive: false}
	s := probe(context.Background(), r, prev)
	if !s.Alive {
		t.Fatalf("alive=false after recovery, err=%q", s.ProbeError)
	}
	if s.SessionsCount != 3 {
		t.Fatalf("enrichment did not run on transition; sessions=%d", s.SessionsCount)
	}
	if s.LastEnrichedAt == "" {
		t.Fatalf("LastEnrichedAt not recorded")
	}
}

// TestProbeFastPathFailureFlipsAliveImmediately confirms that when the
// /health endpoint is unreachable, probe() returns Alive=false without
// waiting for the CLI — that is the whole point of the fast path. Even
// if a hypothetical CLI succeeds, we do not upgrade the verdict: the UI
// hits /health on the same port as we do, so a /health that does not
// answer means the gateway is not usable from the UI's perspective.
func TestProbeFastPathFailureFlipsAliveImmediately(t *testing.T) {
	// Unreachable port (1) on loopback fails fast with connection refused.
	withFastEndpoint(t, "http://127.0.0.1:1/")

	// A CLI that would otherwise report ok=true — proves we do NOT let
	// the CLI override the HTTP verdict.
	bin := writeFakeBin(t, `cat <<'EOF'
{"ok":true,"ts":1,"sessions":{"count":0}}
EOF`)
	s := probe(context.Background(), Runner{Binary: bin}, Snapshot{})
	if s.Alive {
		t.Fatalf("alive=true despite /health refused; CLI must not override")
	}
	if s.ProbeError == "" {
		t.Fatalf("probe_error empty on HTTP failure")
	}
}

// TestProbeFastPath5xxFlipsAliveImmediately covers a gateway that is
// bound but returning non-2xx from /health (e.g. mid-boot / crashed
// handler). HTTP is authoritative: Alive=false regardless of CLI.
func TestProbeFastPath5xxFlipsAliveImmediately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "booting", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	withFastEndpoint(t, srv.URL)

	bin := writeFakeBin(t, `echo "Error: gateway unreachable" 1>&2; exit 1`)
	s := probe(context.Background(), Runner{Binary: bin}, Snapshot{})
	if s.Alive {
		t.Fatalf("alive=true despite 503 from /health")
	}
	if s.ProbeError == "" {
		t.Fatalf("probe_error empty on 5xx")
	}
}

// TestProbeFastPathFailureSkipsCLIWhenSteadyDown ensures that on a
// steady-state gateway-down scenario (prev.Alive=false, prior
// enrichment recorded), probe() does NOT spawn the CLI. This is the
// core guarantee that motivated the HTTP fast path: a hung gateway
// must not drag every poll tick through a 12-second CLI timeout.
func TestProbeFastPathFailureSkipsCLIWhenSteadyDown(t *testing.T) {
	withFastEndpoint(t, "http://127.0.0.1:1/")

	// CLI points at /bin/false — any invocation would be detectable via
	// the probe's LastProbeMS (which in authoritative mode picks up the
	// CLI run duration). In enrich-only mode it stays at the HTTP value.
	// To make the skip assertion robust we use a CLI that, if run, would
	// sleep 500 ms and then exit 0 — LastProbeMS well under that proves
	// it was not executed.
	bin := writeFakeBin(t, `sleep 0.5; echo '{"ok":true}'`)

	prev := Snapshot{
		Alive:          false,
		LastEnrichedAt: rfc3339(time.Now().UTC().Add(-10 * time.Second)),
	}
	start := time.Now()
	s := probe(context.Background(), Runner{Binary: bin}, prev)
	elapsed := time.Since(start)

	if s.Alive {
		t.Fatalf("alive flipped true unexpectedly")
	}
	if elapsed >= 400*time.Millisecond {
		t.Fatalf("probe took %v — CLI was spawned despite steady-down state", elapsed)
	}
}
