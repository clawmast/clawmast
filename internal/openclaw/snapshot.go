package openclaw

import (
	"sync"
	"time"
)

// Snapshot is the in-memory summary of openclaw's latest state, as
// observed by the most recent health probe. Fields are deliberately
// flat JSON for direct wire exposure on /api/openclaw/status. All time
// fields are UTC RFC 3339 strings; zero values are rendered as "".
type Snapshot struct {
	// Phase is the computed lifecycle state — the single source of
	// truth for the UI badge. Derived from Probed/Alive/Intent/
	// CLIMissing plus the Manager's currentAction and
	// lastStartAttemptAt via ComputePhase. Exposed on the wire so the
	// UI can switch on it directly without re-deriving the decision
	// table client-side (which historically caused Probed/Alive/Intent
	// to drift out of agreement after refactors). Survives page
	// refreshes because the warmup-grace timestamp lives on the
	// backend. See phase.go for the full decision matrix.
	Phase Phase `json:"phase"`
	// Probed is true once the poller has written at least once. When
	// false, the UI renders "checking..." rather than "down".
	Probed bool `json:"probed"`
	// Alive reflects whether the most recent `openclaw health --json`
	// call succeeded and reported ok=true. This is the signal for the
	// status badge dot.
	Alive bool `json:"alive"`
	// CLIMissing is true when the openclaw binary was not found on
	// PATH. The UI renders an install-openclaw hint in that case and
	// disables the Fix button (nothing to fix through).
	CLIMissing bool `json:"cli_missing"`
	// BinaryPath is the absolute path of the openclaw executable the
	// probe last ran (or would have run) — empty when discovery fell
	// through to SourceNone. Exposed so the UI System card can render
	// the exact copy being driven, which is surprisingly non-obvious
	// on hosts that carry several Node toolchains side by side.
	BinaryPath string `json:"binary_path,omitempty"`
	// BinarySource labels the discovery strategy that produced
	// BinaryPath (one of openclaw.DiscoverySource). The UI pairs this
	// with BinaryPath to show "found via brew" / "found via fnm".
	BinarySource string `json:"binary_source,omitempty"`
	// ProbeError carries the most recent failure reason (timeout,
	// non-zero exit, parse error, ...) so the UI can render something
	// more actionable than a silent red dot. Empty on success.
	ProbeError string `json:"probe_error,omitempty"`
	// LastProbeAt is when the most recent probe finished (success or
	// failure). Used for "last checked 3s ago" copy.
	LastProbeAt string `json:"last_probe_at,omitempty"`
	// LastProbeMS is the spawn+parse duration in milliseconds, handy
	// for diagnosing a slow gateway without reading logs.
	LastProbeMS int64 `json:"last_probe_ms"`
	// LastAliveAt records the last time we saw Alive=true. Not reset
	// on subsequent failure so the UI can show "down for 2m".
	LastAliveAt string `json:"last_alive_at,omitempty"`
	// Health fields mirror the small subset of `openclaw health --json`
	// that the dashboard actually renders today. Upstream adds fields
	// (sessions, agents, channels, ...) freely; we only promise to
	// expose a minimal set and pass through the raw payload for callers
	// who want more.
	HealthTS         int64  `json:"health_ts,omitempty"`
	HealthDurationMS int64  `json:"health_duration_ms,omitempty"`
	DefaultAgentID   string `json:"default_agent_id,omitempty"`
	HeartbeatSeconds int    `json:"heartbeat_seconds,omitempty"`
	SessionsCount    int    `json:"sessions_count"`
	// ChannelCount is the number of configured channels. Zero is
	// valid ("gateway up, no channels onboarded yet"), so the UI must
	// not treat zero as an error.
	ChannelCount int `json:"channel_count"`
	// Intent records the user's last observed desire for the gateway's
	// run state: "running" after start / restart / fix, "stopped"
	// after stop. Doctor does not touch it (read-only). Empty on boot
	// before any user action — treated as "running" by the UI so a
	// fresh install with a broken gateway renders as 异常 (abnormal)
	// rather than misleading the operator into thinking it was a
	// deliberate stop.
	//
	// Intent is process-local; it does not persist across clawmast
	// restarts. This is deliberate — cross-restart intent would need a
	// disk write per click and still get stale the moment a sibling
	// process (launchd, another operator) intervenes.
	Intent Intent `json:"intent,omitempty"`

	// CurrentAction mirrors Manager.currentAction on the wire so the UI
	// can derive button-loading state from the Snapshot alone. A page
	// refresh mid-restart used to drop the per-button spinner because
	// the JS activeAction variable was pure local state; exposing the
	// in-flight action here lets renderOpenClaw re-adopt the pending
	// paint for any operator (same tab after refresh, cross-tab, cold
	// load) as long as the backend RunAction is still executing.
	//
	// Populated on every probe tick and overlaid by Manager.Get so the
	// field reflects setCurrentAction writes that land between ticks,
	// which matters: a click's first /status after the action starts
	// must already see the name or the UI shows 运行中 for up to one
	// probe interval before adopting.
	//
	// Empty means no action is in flight. The value is one of
	// "start" / "stop" / "restart" / "doctor" / "fix".
	CurrentAction string `json:"current_action,omitempty"`

	// LastEnrichedAt is the RFC 3339 timestamp of the most recent CLI
	// enrichment (`openclaw health --json`) that succeeded. Used by
	// probe() to throttle the slow CLI spawn: liveness is driven by the
	// gateway's own HTTP /health endpoint (~10 ms) on every tick, while
	// the CLI only runs on state transitions or when this cache ages
	// past EnrichmentInterval. Not on the wire — purely a rate-limiting
	// bookkeeping field that lives on the in-memory Snapshot.
	LastEnrichedAt string `json:"-"`

	// Gateway* fields surface the resolved listen address — where we
	// actually sent the /health probe this tick. Populated from the
	// shared GatewayAddress cache (gateway_addr.go) so the UI can show
	// the live port instead of hardcoding 127.0.0.1:18789 and so an
	// operator who bumps the port via openclaw.json or `openclaw
	// gateway --port NNN` sees the dashboard pick it up after the next
	// resolver refresh (≤ AddressRefreshInterval, 5 min today).
	GatewayHost         string `json:"gateway_host,omitempty"`
	GatewayPort         int    `json:"gateway_port,omitempty"`
	GatewayPortSource   string `json:"gateway_port_source,omitempty"`
	GatewayAddrResolved string `json:"gateway_addr_resolved_at,omitempty"`

	// GatewayPID is service.runtime.pid from the resolver, 0 when the
	// upstream reports the service as stopped or when we have not
	// resolved yet. GatewayPIDSince is our observed RFC3339 timestamp
	// for the first probe that saw this PID — it resets whenever PID
	// changes to a new non-zero value. The UI derives uptime as
	// "now − pid_since" and phrases it as "seen running for X" to be
	// honest about the fact that it resets on clawmast restart. PID
	// is not carried on the wire as an action target — it is purely a
	// rendering hint.
	GatewayPID      int    `json:"gateway_pid,omitempty"`
	GatewayPIDSince string `json:"gateway_pid_since,omitempty"`

	// CrashCount tallies alive=true→false transitions observed by the
	// poller since the clawmast process started. Counts only edges,
	// not sustained-down ticks, and never decrements. Resets to 0 on
	// clawmast restart (it is in-memory only). The UI surfaces this
	// in the diagnostic dialog so an operator glancing at a flapping
	// gateway can tell "it crashed 4 times in 5 minutes" from "it
	// just went down once and is still down".
	CrashCount int `json:"crash_count"`

	// ProbeHistory is a rolling ring of the most recent probe samples,
	// newest-last. Bounded by ProbeHistoryLen so the JSON payload
	// stays tiny even over a long session. The UI plots probe_ms as a
	// sparkline with red markers for alive=false ticks.
	ProbeHistory []ProbeSample `json:"probe_history,omitempty"`

	// AutohealEnabled mirrors AutohealConfig.Enabled so the UI can
	// show a row in the diagnostic dialog. Always on the wire (even
	// when false) so a future UI toggle could key on it; the renderer
	// today hides the row when disabled.
	AutohealEnabled bool `json:"autoheal_enabled"`
	// AutohealCount tallies the number of autoheal triggers fired
	// since the clawmast process started (in-memory, resets on
	// restart — same semantics as CrashCount).
	AutohealCount int `json:"autoheal_count"`
	// AutohealLastAt is the RFC 3339 timestamp of the most recent
	// autoheal trigger. Empty when autoheal has never fired.
	AutohealLastAt string `json:"autoheal_last_at,omitempty"`
	// AutohealNextEligibleAt is the RFC 3339 timestamp at which the
	// cooldown expires and autoheal is eligible to fire again. Empty
	// before the first trigger. The UI derives "cooldown remaining"
	// as (next_eligible − now).
	AutohealNextEligibleAt string `json:"autoheal_next_eligible_at,omitempty"`

	// AutohealConsecutiveDown counts back-to-back alive=false probes
	// since the last alive=true tick. Reset to 0 on any alive probe
	// and on autoheal trigger. Internal bookkeeping — not on the wire.
	AutohealConsecutiveDown int `json:"-"`
	// AutohealNextEligible is the authoritative time.Time form of
	// AutohealNextEligibleAt. Kept alongside the string so tick-time
	// cooldown checks do not re-parse RFC3339 on every probe.
	AutohealNextEligible time.Time `json:"-"`
}

// ProbeSample is one entry in Snapshot.ProbeHistory. Kept as plain
// fields rather than a richer struct so the JSON on /api/openclaw/
// status reads naturally in a browser inspector.
type ProbeSample struct {
	At      string `json:"at"`
	Alive   bool   `json:"alive"`
	ProbeMS int64  `json:"probe_ms"`
}

// ProbeHistoryLen bounds Snapshot.ProbeHistory. 20 samples at the
// active cadence (1 s) covers the last ≈20 seconds of behaviour,
// which is long enough for the UI to show a meaningful trend without
// bloating every /status response.
const ProbeHistoryLen = 20

// Intent is the operator's stated desire for the gateway's run state.
// Used to distinguish "已停止" (user stopped it) from "异常" (it crashed
// or never came up) when the probe reports !alive. See Snapshot.Intent.
type Intent string

const (
	IntentUnknown Intent = ""
	IntentRunning Intent = "running"
	IntentStopped Intent = "stopped"
)

// healthPayload matches the fields we care about in `openclaw health
// --json`. Any field not listed here is ignored; we never roundtrip
// unknown fields back to the UI (the wire shape is our stable API,
// not openclaw's).
type healthPayload struct {
	OK               bool   `json:"ok"`
	TS               int64  `json:"ts"`
	DurationMS       int64  `json:"durationMs"`
	DefaultAgentID   string `json:"defaultAgentId"`
	HeartbeatSeconds int    `json:"heartbeatSeconds"`
	Sessions         struct {
		Count int `json:"count"`
	} `json:"sessions"`
	Channels map[string]any `json:"channels"`
}

// store holds the current Snapshot behind an RWMutex. All access goes
// through Get/set so the poller's writes and the HTTP handler's reads
// never share a mutable pointer.
type store struct {
	mu   sync.RWMutex
	snap Snapshot
}

func (s *store) Get() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap
}

func (s *store) set(mut func(*Snapshot)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mut(&s.snap)
}

// rfc3339 returns t.UTC().Format(time.RFC3339Nano) but returns "" on
// the zero time so JSON omitempty can drop the field.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
