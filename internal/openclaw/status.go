package openclaw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ProbeTimeout bounds one `openclaw health --json` CLI spawn. Upstream
// advertises a 10s internal timeout (docs/cli/health.md), so we allow
// a small margin above that before SIGKILLing for a #11843 hang. This
// is the slow path — the normal tick hits the HTTP /health endpoint in
// ~10 ms and never spawns Node.
const ProbeTimeout = 12 * time.Second

// LivenessTimeout bounds one HTTP GET to the gateway's /health endpoint.
// The endpoint is hot in Node's event loop (no DB, no FS, no upstream
// calls), so anything over a second means the gateway is hung and should
// be treated as not alive.
const LivenessTimeout = 1500 * time.Millisecond

// EnrichmentInterval is the minimum gap between CLI-based enrichment
// spawns while the gateway is steadily alive. Below this threshold we
// skip the CLI and let the HTTP liveness probe drive the Snapshot. Thirty
// seconds is short enough that channel/session counts stay fresh on a
// human-perceptible timescale, and long enough to keep Node spawn noise
// roughly one order of magnitude below the pre-HTTP-probe baseline.
const EnrichmentInterval = 30 * time.Second

// probeFastEndpoint, when non-empty, overrides the URL the fast
// liveness probe hits. Production leaves it empty and lets the probe
// read the address from the runtime cache (gateway_addr.go), which is
// kept current by a background resolver running `openclaw gateway
// status --json`. Tests that cover the HTTP path set it to a
// httptest.Server URL; tests that need CLI-authoritative behaviour set
// probeFastDisabled to true (see TestMain).
var probeFastEndpoint = ""

// probeFastDisabled, when true, forces probe() down the CLI-
// authoritative path regardless of the cache or the override URL.
// Only tests should flip this — production always wants the fast path.
var probeFastDisabled = false

// livenessClient is reused across probes so a successful TCP connection
// can be kept alive for subsequent ticks. Timeouts are set on the client
// rather than per-request because all liveness calls share the same
// budget. IdleConnTimeout intentionally exceeds IdlePollInterval so the
// connection survives idle periods without re-handshaking.
var livenessClient = &http.Client{
	Timeout: LivenessTimeout,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   LivenessTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: LivenessTimeout,
		ExpectContinueTimeout: 100 * time.Millisecond,
	},
}

// healthLiveBody is the canonical "gateway up" payload from /health. We
// do not require a strict match: any successful 2xx JSON body with ok=true
// is accepted. The var is only kept for documentation.

// probeFastResult carries the verdict of one HTTP liveness probe. ok is
// true when the gateway responded with HTTP 2xx and ok=true in the body;
// failure modes (dial refused, timeout, non-200, ok=false) all set ok to
// false and errMsg to a human-readable summary.
type probeFastResult struct {
	ok       bool
	duration time.Duration
	errMsg   string
}

// probeFast runs the cheap HTTP liveness probe against the gateway's
// own /health endpoint. It never returns a nil result — failures are
// captured in errMsg. Callers treat an ok=true result as authoritative
// for the Alive field; an ok=false result means they must fall back to
// the CLI (which also discriminates "gateway down" from "CLI missing").
func probeFast(ctx context.Context) probeFastResult {
	if probeFastDisabled {
		return probeFastResult{ok: false, errMsg: "liveness probe disabled"}
	}
	url := probeFastEndpoint
	if url == "" {
		url = LoadGatewayAddress().HTTPHealthURL()
	}
	reqCtx, cancel := context.WithTimeout(ctx, LivenessTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return probeFastResult{ok: false, errMsg: err.Error()}
	}
	start := time.Now()
	resp, err := livenessClient.Do(req)
	dur := time.Since(start)
	if err != nil {
		return probeFastResult{ok: false, duration: dur, errMsg: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return probeFastResult{ok: false, duration: dur, errMsg: fmt.Sprintf("http %d", resp.StatusCode)}
	}
	// Body is tiny (~30 bytes); cap the read to guard against a hung
	// gateway streaming garbage into the socket.
	var body struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 512)).Decode(&body); err != nil {
		return probeFastResult{ok: false, duration: dur, errMsg: "parse /health: " + err.Error()}
	}
	if !body.OK {
		return probeFastResult{ok: false, duration: dur, errMsg: "gateway /health reports ok=false"}
	}
	return probeFastResult{ok: true, duration: dur}
}

// needsEnrichment decides whether the CLI-based enrichment probe should
// run for this tick. CLI is never authoritative for Alive in the HTTP
// mode — it exists purely to populate channel / session / agent counts
// that the cheap /health endpoint does not expose. Returns true when:
//
//   - We have no prior enrichment record (first boot — we also need
//     this to discover cli_missing once, even if the gateway is down).
//   - The gateway just transitioned from !alive to alive (so the UI
//     sees fresh channel / session counts within one tick of recovery).
//   - We are steadily alive and the cached enrichment has aged past
//     EnrichmentInterval.
//
// When the gateway is steadily down, we skip the CLI — calling it would
// try to connect to the same hung gateway and block for ProbeTimeout,
// which is exactly what the HTTP fast path is supposed to avoid.
func needsEnrichment(prev Snapshot, nowAlive bool, now time.Time) bool {
	if prev.LastEnrichedAt == "" {
		return true
	}
	if nowAlive && !prev.Alive {
		return true
	}
	if !nowAlive {
		return false
	}
	prevAt, err := time.Parse(time.RFC3339, prev.LastEnrichedAt)
	if err != nil {
		return true
	}
	return now.Sub(prevAt) >= EnrichmentInterval
}

// probe runs one health check and returns an updated Snapshot built
// from the previous one (so sticky fields like LastAliveAt are
// preserved across transient failures). probe never panics and always
// returns a fully-populated Snapshot — errors are recorded in
// Snapshot.ProbeError, not returned.
//
// Two-tier strategy:
//  1. Cheap HTTP GET /health (~10 ms) is the sole authority for Alive.
//     A timed-out or refused probe flips Alive=false immediately so
//     the UI can react within one poll tick (≈1 s) of a hang.
//  2. CLI `openclaw health --json` runs only for enrichment (channels /
//     sessions / agent) and for cli-missing detection. It is rate-
//     limited by needsEnrichment and never changes the Alive verdict
//     already set by the HTTP probe.
//
// When probeFastEndpoint is empty (test harness) the CLI becomes the
// authority — exercised by the pre-HTTP test suite in status_test.go.
func probe(ctx context.Context, runner Runner, prev Snapshot) Snapshot {
	next := prev
	next.Probed = true
	now := time.Now().UTC()
	next.LastProbeAt = rfc3339(now)

	// Snapshot the resolved gateway address every tick so the UI can
	// render the live host/port pair (the resolver runs asynchronously
	// on its own schedule; reading the cache here is lock-free).
	addr := LoadGatewayAddress()
	next.GatewayHost = addr.Host
	next.GatewayPort = addr.Port
	next.GatewayPortSource = addr.Source
	next.GatewayAddrResolved = rfc3339(addr.ResolvedAt)

	if probeFastDisabled {
		// Test path: CLI drives every field, Alive included.
		return probeEnrichCLI(ctx, runner, next, now, cliAuthoritative)
	}

	fast := probeFast(ctx)
	if fast.ok {
		next.Alive = true
		next.ProbeError = ""
		next.LastProbeMS = fast.duration.Milliseconds()
		next.LastAliveAt = rfc3339(now)
	} else {
		next.Alive = false
		next.ProbeError = fast.errMsg
		next.LastProbeMS = fast.duration.Milliseconds()
	}
	if needsEnrichment(prev, next.Alive, now) {
		next = probeEnrichCLI(ctx, runner, next, now, cliEnrichOnly)
	}
	return next
}

// cliRole selects how probeEnrichCLI interprets its result. In
// cliAuthoritative mode (HTTP probe disabled) it writes Alive and
// ProbeError from the CLI exit code. In cliEnrichOnly mode the HTTP
// probe already decided Alive; the CLI only contributes enrichment
// fields and the cli_missing flag.
type cliRole int

const (
	cliEnrichOnly cliRole = iota
	cliAuthoritative
)

// probeEnrichCLI runs `openclaw health --json` and merges its payload
// into s. Behaviour depends on role:
//
//   - cliEnrichOnly: success populates channel/session/agent fields;
//     failure only sets the cli_missing flag. Alive and ProbeError
//     stay as HTTP left them.
//   - cliAuthoritative: every CLI outcome (exit code, parse error,
//     ok=false) overwrites Alive and ProbeError. Used by tests and by
//     the legacy no-HTTP path.
func probeEnrichCLI(ctx context.Context, runner Runner, s Snapshot, now time.Time, role cliRole) Snapshot {
	res, err := runner.runCmd(ctx, ProbeTimeout, "health", "--json")
	if errors.Is(err, ErrBinaryMissing) {
		s.CLIMissing = true
		if role == cliAuthoritative {
			s.Alive = false
			s.ProbeError = "openclaw binary not found on PATH"
			s.LastProbeMS = 0
		}
		return s
	}
	s.CLIMissing = false
	if role == cliAuthoritative && res != nil {
		s.LastProbeMS = res.Duration.Milliseconds()
	}
	if err != nil {
		if role == cliAuthoritative {
			s.Alive = false
			s.ProbeError = err.Error()
		}
		return s
	}
	if res.ExitCode != 0 {
		if role == cliAuthoritative {
			s.Alive = false
			// Surface the tail of stderr so the UI can render something
			// more actionable than "exit 1". Capping at 400 bytes keeps
			// response size bounded.
			s.ProbeError = fmt.Sprintf("exit %d: %s", res.ExitCode, trimErr(res.Stderr))
		}
		return s
	}
	var payload healthPayload
	if err := json.Unmarshal(res.Stdout, &payload); err != nil {
		if role == cliAuthoritative {
			s.Alive = false
			s.ProbeError = fmt.Sprintf("parse health json: %v", err)
		}
		return s
	}
	if !payload.OK {
		if role == cliAuthoritative {
			s.Alive = false
			s.ProbeError = "gateway reports ok=false"
		}
		return s
	}
	if role == cliAuthoritative {
		s.Alive = true
		s.ProbeError = ""
		s.LastAliveAt = rfc3339(now)
	}
	s.HealthTS = payload.TS
	s.HealthDurationMS = payload.DurationMS
	s.DefaultAgentID = payload.DefaultAgentID
	s.HeartbeatSeconds = payload.HeartbeatSeconds
	s.SessionsCount = payload.Sessions.Count
	s.ChannelCount = len(payload.Channels)
	s.LastEnrichedAt = rfc3339(now)
	return s
}

// trimErr keeps the last ~400 bytes of stderr, trimmed. The tail is
// usually where Node prints the actual error after a stack trace.
func trimErr(b []byte) string {
	const cap = 400
	if len(b) <= cap {
		return strings.TrimSpace(string(b))
	}
	return strings.TrimSpace(string(b[len(b)-cap:]))
}
