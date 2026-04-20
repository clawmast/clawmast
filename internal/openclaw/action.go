package openclaw

import (
	"context"
	"fmt"
	"time"
)

// Action names the single-step gateway operations the dashboard exposes
// alongside the Fix cascade. The wire values here must match the ?name=
// query parameter the UI posts to /api/openclaw/action.
type Action string

const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionRestart Action = "restart"
	// ActionDoctor runs a read-only diagnose (no --fix) so the operator
	// can see what the cascade would touch before triggering it.
	ActionDoctor Action = "doctor"
)

// ValidAction returns true for action names the dispatcher knows how to
// run. The handler uses this to reject unknown ?name= values with 400
// instead of attempting an openclaw invocation.
func ValidAction(name string) bool {
	switch Action(name) {
	case ActionStart, ActionStop, ActionRestart, ActionDoctor:
		return true
	}
	return false
}

// actionTimeout per-action wallclock budgets. Start/Stop/Restart all
// drive the gateway which upstream advertises bounded work; Doctor can
// do a little more because it probes providers too.
func actionTimeout(a Action) time.Duration {
	switch a {
	case ActionStart, ActionRestart:
		return 20 * time.Second
	case ActionStop:
		return 10 * time.Second
	case ActionDoctor:
		return 30 * time.Second
	default:
		return 15 * time.Second
	}
}

// actionArgv maps an Action to the openclaw CLI arguments. Centralised
// here so new actions only need one add-site and the handler stays a
// pure dispatcher.
func actionArgv(a Action) []string {
	switch a {
	case ActionStart:
		return []string{"gateway", "start"}
	case ActionStop:
		return []string{"gateway", "stop"}
	case ActionRestart:
		return []string{"gateway", "restart"}
	case ActionDoctor:
		// Read-only; --fix is reserved for the Fix cascade.
		return []string{"doctor"}
	}
	return nil
}

// actionStepID stabilises the StepID wire value per action so the UI
// can key row identity on something other than the command string.
func actionStepID(a Action) StepID {
	return StepID("action-" + string(a))
}

// RunAction executes one openclaw CLI command (non-cascade) and emits
// start/end StepEvents on evCh. The channel is closed when the action
// finishes. The final event carries a post-action probe in Snapshot form
// when includeProbe is true; the HTTP handler uses that to deliver the
// fresh state in the same body instead of forcing a follow-up GET.
//
// Callers that want the gateway state to reflect the action (all four
// verbs) should pass includeProbe=true. The probe runs with a 5s
// timeout inside the remaining request context.
func RunAction(ctx context.Context, m *Manager, evCh chan<- StepEvent, a Action) {
	defer close(evCh)
	// Record the in-flight action by name so the probe loop holds
	// autoheal off (any non-empty value qualifies) and ComputePhase
	// can elect PhaseStarting for start/restart specifically. Cleared
	// via defer so a panic in runCmd still restores it — a stuck
	// non-empty value would permanently disable autoheal and keep
	// the badge blue forever.
	m.setCurrentAction(string(a))
	defer m.clearCurrentAction()
	if !ValidAction(string(a)) {
		evCh <- StepEvent{
			Step:    StepID("action-" + string(a)),
			Phase:   "end",
			Outcome: OutcomeFailed,
			Note:    fmt.Sprintf("unknown action %q", string(a)),
		}
		return
	}

	id := actionStepID(a)
	argv := actionArgv(a)
	cmdStr := joinArgv(append([]string{"openclaw"}, argv...))
	evCh <- StepEvent{Step: id, Phase: "start", Command: cmdStr}

	runner, _ := m.resolveRunner()
	res, err := runner.runCmd(ctx, actionTimeout(a), argv...)
	outcome := OutcomeOK
	note := ""
	if err != nil {
		if res != nil && res.Killed {
			outcome = OutcomeTimeout
		} else {
			outcome = OutcomeFailed
		}
		note = err.Error()
	} else if res != nil && res.ExitCode != 0 {
		outcome = OutcomeFailed
	}
	end := StepEvent{Step: id, Phase: "end", Outcome: outcome, Command: cmdStr, Note: note}
	if res != nil {
		end.DurationMS = res.Duration.Milliseconds()
		end.ExitCode = res.ExitCode
		end.StdoutTail = trimErr(res.Stdout)
		end.StderrTail = trimErr(res.Stderr)
	}
	evCh <- end

	// Stamp the warmup-grace anchor for any start/restart attempt,
	// regardless of the CLI's own exit code. `openclaw gateway
	// restart` runs a post-restart health self-check that races the
	// gateway's HTTP warmup — the CLI routinely exits non-zero even
	// when the gateway will be alive a few seconds later. Gating the
	// stamp on OutcomeOK dropped us into PhaseError for the 5–15 s
	// warmup window in exactly those cases, defeating the grace
	// period that ComputePhase was built for. The anchor only opens
	// a 15 s leniency window; a genuine failure still surfaces when
	// no alive probe lands before it expires. Stop/doctor remain
	// excluded: stop inverts intent, doctor is read-only.
	if a == ActionStart || a == ActionRestart {
		m.stampStartAttempt(time.Now().UTC())
	}

	// Refresh the snapshot so the subsequent /api/openclaw/status poll
	// in the UI reflects the post-action state without waiting for the
	// 3-5s poll tick. Budget matches ProbeTimeout because a freshly
	// restarted gateway can take most of the health window to warm up;
	// clipping this to anything shorter caused post-restart probes to
	// be killed mid-flight and made successful restarts render as
	// "重启失败" in the action console (while the background poller
	// immediately saw alive=true on the next tick — a self-contradicting
	// UI state).
	pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	m.ProbeNow(pctx)
}
