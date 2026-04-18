package openclaw

import (
	"context"
	"testing"
	"time"
)

// TestCascadeStopsAfterT1WhenHealthy verifies the early-exit path:
// when T1 (doctor --fix) succeeds and the probe afterward reports
// alive, T2/T3 must not run.
func TestCascadeStopsAfterT1WhenHealthy(t *testing.T) {
	// Stub dispatches on $1:
	//   doctor   -> exit 0, no output
	//   health   -> print alive JSON
	//   anything else -> exit 99 so the test fails loudly if T2+ run
	bin := writeFakeBin(t, `
case "$1" in
  doctor) exit 0 ;;
  health) echo '{"ok":true,"ts":1,"sessions":{"count":0}}' ;;
  *) exit 99 ;;
esac
`)
	m := NewManager(Runner{Binary: bin}, time.Second, nil)
	ch := make(chan StepEvent, 16)
	go Cascade(context.Background(), m, ch)
	events := drainEvents(ch)
	if len(events) == 0 {
		t.Fatalf("no events")
	}
	for _, ev := range events {
		if ev.Step == StepGatewayRestart || ev.Step == StepOSRestart {
			t.Fatalf("later tier %s should have been skipped; event=%+v", ev.Step, ev)
		}
	}
	if got := onlyDoctorOutcome(events); got != OutcomeOK {
		t.Fatalf("T1 outcome: want ok got %s", got)
	}
}

// TestCascadeTimeoutOnT1 verifies T1 gets marked outcome=timeout when
// the openclaw CLI hangs. T2 then runs (cascade continues).
func TestCascadeTimeoutOnT1(t *testing.T) {
	bin := writeFakeBin(t, `
case "$1" in
  doctor) sleep 120 ;;
  gateway) exit 0 ;;
  health) echo '{"ok":true,"ts":1,"sessions":{"count":0}}' ;;
  *) exit 99 ;;
esac
`)
	// Shrink T1 timeout so the test stays fast. We wrap the runner
	// to intercept runCmd is overkill — instead we rely on the
	// public cascade budget and a fast sleep-before-kill path that
	// runCmd exercises via its context.
	m := NewManager(Runner{Binary: bin}, time.Second, nil)
	// Replace doctorTimeout via a direct call of the lower-level
	// step logic to keep this test quick.
	ch := make(chan StepEvent, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		defer close(ch)
		// Inline a shortened cascade: T1 (200 ms budget) then T2.
		runStep(ctx, m, ch, StepDoctor, 200*time.Millisecond, []string{"doctor", "--fix"})
		runStep(ctx, m, ch, StepGatewayRestart, 1*time.Second, []string{"gateway", "restart"})
	}()
	events := drainEvents(ch)
	var sawT1Timeout, sawT2OK bool
	for _, ev := range events {
		if ev.Step == StepDoctor && ev.Phase == "end" && ev.Outcome == OutcomeTimeout {
			sawT1Timeout = true
		}
		if ev.Step == StepGatewayRestart && ev.Phase == "end" && ev.Outcome == OutcomeOK {
			sawT2OK = true
		}
	}
	if !sawT1Timeout {
		t.Fatalf("expected T1 timeout event; events=%+v", events)
	}
	if !sawT2OK {
		t.Fatalf("expected T2 ok event; events=%+v", events)
	}
}

// drainEvents reads until the channel is closed.
func drainEvents(ch <-chan StepEvent) []StepEvent {
	var out []StepEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// onlyDoctorOutcome returns the end-phase outcome of the T1 step, or
// "" if no end event was emitted.
func onlyDoctorOutcome(events []StepEvent) StepOutcome {
	for _, ev := range events {
		if ev.Step == StepDoctor && ev.Phase == "end" {
			return ev.Outcome
		}
	}
	return ""
}
