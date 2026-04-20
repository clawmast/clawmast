package openclaw

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"
)

// StepID identifies each tier of the Fix cascade. Wire values are
// stable so the UI can key on them. See refactor.md §10 decision #20.
type StepID string

const (
	StepDoctor         StepID = "t1-doctor"
	StepGatewayRestart StepID = "t2-gateway-restart"
	StepOSRestart      StepID = "t3-os-restart"
)

// StepOutcome is the result classification for one cascade step.
type StepOutcome string

const (
	OutcomeOK      StepOutcome = "ok"
	OutcomeTimeout StepOutcome = "timeout"
	OutcomeFailed  StepOutcome = "failed"
	OutcomeSkipped StepOutcome = "skipped"
)

// StepEvent is emitted on the cascade channel as each tier runs.
// "start" events have Outcome=="" and no timing data; "end" events
// carry Outcome plus Stdout/Stderr tails. Duration is always the
// end-to-end wallclock of that step.
type StepEvent struct {
	Step       StepID      `json:"step"`
	Phase      string      `json:"phase"` // "start" | "end"
	Outcome    StepOutcome `json:"outcome,omitempty"`
	Command    string      `json:"command,omitempty"`
	DurationMS int64       `json:"duration_ms,omitempty"`
	ExitCode   int         `json:"exit_code,omitempty"`
	StdoutTail string      `json:"stdout_tail,omitempty"`
	StderrTail string      `json:"stderr_tail,omitempty"`
	Note       string      `json:"note,omitempty"`
}

// Per-step timeouts match refactor.md §10 decision #20.
const (
	doctorTimeout    = 30 * time.Second
	gatewayTimeout   = 15 * time.Second
	osRestartTimeout = 10 * time.Second
)

// Cascade runs T1 → T2 → T3 sequentially, emitting StepEvents on evCh.
// It early-exits after a tier if probe() reports the gateway alive,
// because later tiers would be redundant. evCh is closed by Cascade
// when it finishes. The caller is expected to read evCh in a goroutine
// or the writes will block — for the HTTP handler that's fine because
// it streams straight into an NDJSON body.
//
// Marks currentAction="fix" for the cascade's lifetime so (a) autoheal
// stands down while a fix is in progress (prevents an autoheal re-fire
// racing with the user's click) and (b) ComputePhase elects
// PhaseStarting so the badge stays blue/breathing through the cascade
// instead of flashing 异常 between T1's restart and T1's readiness.
func Cascade(ctx context.Context, m *Manager, evCh chan<- StepEvent) {
	defer close(evCh)

	m.setCurrentAction("fix")
	defer m.clearCurrentAction()
	// finalizeFixLog stamps the log's Running/Outcome mirroring the
	// handler's finalOutcome: "ok" when the gateway is alive after
	// the cascade, "failed" otherwise. Called on every exit path
	// (early T1/T2 health hit and the T3 fall-through) so a replay
	// client always sees a settled entry once CurrentAction clears.
	finalizeFixLog := func() {
		outcome := "failed"
		if m.Get().Alive {
			outcome = "ok"
		}
		m.FinalizeActionLog(outcome)
	}

	runStep(ctx, m, evCh, StepDoctor, doctorTimeout, []string{"doctor", "--fix"})
	if healthyAfter(ctx, m) {
		m.stampStartAttempt(time.Now().UTC())
		finalizeFixLog()
		return
	}

	runStep(ctx, m, evCh, StepGatewayRestart, gatewayTimeout, []string{"gateway", "restart"})
	if healthyAfter(ctx, m) {
		m.stampStartAttempt(time.Now().UTC())
		finalizeFixLog()
		return
	}

	// T3 is OS-specific and does not go through the openclaw CLI.
	argv, note := osRestartCommand()
	if argv == nil {
		startEv := StepEvent{Step: StepOSRestart, Phase: "start", Command: "", Note: note}
		m.RecordActionEvent(startEv)
		evCh <- startEv
		endEv := StepEvent{Step: StepOSRestart, Phase: "end", Outcome: OutcomeSkipped, Note: note}
		m.RecordActionEvent(endEv)
		evCh <- endEv
		finalizeFixLog()
		return
	}
	runSystemStep(ctx, m, evCh, StepOSRestart, osRestartTimeout, argv)
	// Final probe regardless of T3 exit code so the UI sees the real
	// state when the cascade finishes. Stamp unconditionally: T3 is a
	// "best effort revive", and the warmup window is exactly where we
	// want the UI to stay blue while the OS restart settles — the next
	// probe will promote to PhaseRunning on success or the grace
	// expires into PhaseError on genuine failure.
	m.stampStartAttempt(time.Now().UTC())
	m.ProbeNow(ctx)
	finalizeFixLog()
}

// runStep spawns an openclaw CLI command with the given timeout and
// emits start/end events. The end event's Outcome is OK for exit 0,
// Timeout for killed-by-ctx-deadline, and Failed otherwise.
func runStep(ctx context.Context, m *Manager, evCh chan<- StepEvent, id StepID, timeout time.Duration, args []string) {
	cmdStr := joinArgv(append([]string{"openclaw"}, args...))
	startEv := StepEvent{Step: id, Phase: "start", Command: cmdStr}
	m.RecordActionEvent(startEv)
	evCh <- startEv
	runner, _ := m.resolveRunner()
	res, err := runner.runCmd(ctx, timeout, args...)
	outcome := OutcomeOK
	note := ""
	if err != nil {
		if res != nil && res.Killed {
			outcome = OutcomeTimeout
			note = err.Error()
		} else {
			outcome = OutcomeFailed
			note = err.Error()
		}
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
	m.RecordActionEvent(end)
	evCh <- end
}

// runSystemStep is the T3 variant: uses Runner.runCmd's machinery but
// with an arbitrary binary (launchctl / systemctl) rather than
// openclaw. We inline the exec logic here to avoid tangling it with
// the openclaw-only path above.
func runSystemStep(ctx context.Context, m *Manager, evCh chan<- StepEvent, id StepID, timeout time.Duration, argv []string) {
	r := Runner{Binary: argv[0]}
	cmdStr := joinArgv(argv)
	startEv := StepEvent{Step: id, Phase: "start", Command: cmdStr}
	m.RecordActionEvent(startEv)
	evCh <- startEv
	res, err := r.runCmd(ctx, timeout, argv[1:]...)
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
	m.RecordActionEvent(end)
	evCh <- end
}

// healthyAfter runs an immediate probe and returns true when Alive is
// true, letting the cascade skip later tiers. Bounded short (5s) so a
// stubborn gateway does not delay the next tier.
func healthyAfter(ctx context.Context, m *Manager) bool {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return m.ProbeNow(pctx).Alive
}

// osRestartCommand returns the platform-specific T3 argv plus a
// human-readable note. Returns (nil, reason) on unsupported OSes so
// the cascade emits a skipped event instead of silently no-opping.
func osRestartCommand() ([]string, string) {
	switch runtime.GOOS {
	case "darwin":
		label := "gui/" + strconv.Itoa(os.Getuid()) + "/ai.openclaw.gateway"
		return []string{"launchctl", "kickstart", "-k", label}, ""
	case "linux":
		return []string{"systemctl", "--user", "restart", "openclaw-gateway.service"}, ""
	default:
		return nil, fmt.Sprintf("T3 OS restart not implemented on %s", runtime.GOOS)
	}
}

// joinArgv formats argv for display in the UI.
func joinArgv(argv []string) string {
	out := ""
	for i, a := range argv {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}
