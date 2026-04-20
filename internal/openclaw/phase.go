package openclaw

import "time"

// Phase is the computed lifecycle state of the OpenClaw gateway as
// observed by clawmast. It is the single source of truth for the UI
// badge — callers must not re-derive a phase from Alive / Intent /
// CLIMissing combinations lest the two code paths drift out of
// agreement (which was the concrete bug this type was introduced to
// fix: restart post-probe flapped 重启完成 → 异常 → 运行中 because the
// client-side decision table couldn't see the backend's "CLI just
// finished, warmup in progress" context).
type Phase string

const (
	// PhaseUnknown is the pre-probe state. UI renders 检测中.
	PhaseUnknown Phase = ""
	// PhaseMissing means the openclaw CLI was not found on PATH. The
	// cascade has nothing to call through so action buttons must be
	// disabled.
	PhaseMissing Phase = "missing"
	// PhaseStopped means the operator explicitly asked for the gateway
	// to be down (via the stop button). Honoured over observed Alive
	// to survive the brief window where the port is still draining.
	PhaseStopped Phase = "stopped"
	// PhaseStarting covers two sub-states that look identical from the
	// UI's perspective: (a) a start/restart/fix action is currently
	// running through openclaw CLI, and (b) that CLI has returned OK
	// but the gateway's HTTP /health endpoint has not yet reported
	// alive — the warmup window. Collapsing both under one phase lets
	// the badge stay blue/breathing for the entire "we are trying to
	// bring it up" span without flapping through 异常.
	PhaseStarting Phase = "starting"
	// PhaseRunning means the most recent probe reported alive=true.
	PhaseRunning Phase = "running"
	// PhaseError means the gateway is !alive with no operator intent
	// to keep it down and no in-flight or warmup cover. This is the
	// state that warrants an operator's attention.
	PhaseError Phase = "error"
)

// WarmupGracePeriod is how long after a successful start/restart/fix
// CLI invocation we continue to classify the gateway as PhaseStarting
// even if the HTTP /health probe has not yet reported alive. Gateway
// cold start on macOS (launchd spawn, Node boot, port bind, health
// handler warm-up) routinely takes 10-15s; without this grace the UI
// would flip to 异常 for the 1-3s window between the CLI returning ok
// and the first alive probe landing.
//
// The grace is generous on purpose — a genuine failure still surfaces
// after the window expires, just delayed by at most this duration.
// Making the UI honest at the cost of a ~15s "maybe coming up" frame
// is the explicit trade-off.
const WarmupGracePeriod = 15 * time.Second

// startingActions lists the action names that, while in flight, imply
// the operator is trying to bring the gateway up. Stop and doctor do
// not qualify: stop is covered by Intent=stopped, and doctor is
// read-only so it must not paint the card as "coming up".
var startingActions = map[string]bool{
	"start":   true,
	"restart": true,
	"fix":     true,
}

// ComputePhase is the pure decision function driving Snapshot.Phase.
// Ordering is load-bearing:
//
//  1. Pre-probe short-circuits — we have nothing to decide on yet.
//  2. CLIMissing beats everything else because no action can recover.
//  3. Operator intent wins over observed alive: a stop that has just
//     issued SetIntent(stopped) should render 已停止 immediately even
//     if the port is still accepting connections for another tick.
//  4. alive=true is the happy path.
//  5. Falling through to error requires explicit disqualification
//     from both the in-flight gate and the warmup grace window.
//
// The function is deterministic given its inputs, which lets the test
// suite table-drive the full matrix.
func ComputePhase(s Snapshot, currentAction string, lastStartAt, now time.Time) Phase {
	return computePhaseWithGrace(s, currentAction, lastStartAt, now, WarmupGracePeriod)
}

// computePhaseWithGrace is the grace-parameterised inner. Tests use
// this to force short / long windows without wall-clock sleeps.
func computePhaseWithGrace(s Snapshot, currentAction string, lastStartAt, now time.Time, grace time.Duration) Phase {
	if !s.Probed {
		return PhaseUnknown
	}
	if s.CLIMissing {
		return PhaseMissing
	}
	if s.Intent == IntentStopped {
		return PhaseStopped
	}
	if s.Alive {
		return PhaseRunning
	}
	if startingActions[currentAction] {
		return PhaseStarting
	}
	if !lastStartAt.IsZero() && now.Sub(lastStartAt) < grace {
		return PhaseStarting
	}
	return PhaseError
}
