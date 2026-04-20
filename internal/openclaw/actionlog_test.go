package openclaw

import (
	"context"
	"testing"
	"time"
)

// TestActionLogReplaysCLIEvents verifies the ring buffer captures
// every StepEvent RunAction emits on the channel so a refresh-replay
// client sees the same transcript a live-stream client would. This
// is the load-bearing invariant for the "lost logs on refresh" fix:
// if the buffer and the channel diverge, a refreshed operator sees a
// half-empty console while the happy path looks fine.
func TestActionLogReplaysCLIEvents(t *testing.T) {
	bin := writeFakeBin(t, `
case "$1" in
  gateway)
    case "$2" in
      start) echo "gateway up"; exit 0 ;;
      *) exit 99 ;;
    esac ;;
  health) echo '{"ok":true,"ts":1,"sessions":{"count":0}}' ;;
  *) exit 99 ;;
esac
`)
	m := NewManager(Runner{Binary: bin}, time.Second, nil)
	ch := make(chan StepEvent, 8)
	RunAction(context.Background(), m, ch, ActionStart)
	streamed := drainEvents(ch)

	entry := m.ActionLog()
	if entry.Name != "start" {
		t.Fatalf("entry.Name: want start got %q", entry.Name)
	}
	if entry.Running {
		t.Fatalf("entry.Running: want false after RunAction returns")
	}
	if entry.Outcome != "ok" {
		t.Fatalf("entry.Outcome: want ok got %q", entry.Outcome)
	}
	if len(entry.Events) != len(streamed) {
		t.Fatalf("buffered events=%d streamed=%d (must match)",
			len(entry.Events), len(streamed))
	}
	for i := range streamed {
		if entry.Events[i] != streamed[i] {
			t.Fatalf("event[%d] mismatch: buffered=%+v streamed=%+v",
				i, entry.Events[i], streamed[i])
		}
	}
}

// TestActionLogResetsBetweenActions verifies a second action's log
// does not contain the first action's events. Without the reset in
// setCurrentAction, a refresh after clicking restart-then-stop would
// show the restart transcript prefixed onto the stop console.
func TestActionLogResetsBetweenActions(t *testing.T) {
	bin := writeFakeBin(t, `
case "$1-$2" in
  gateway-start) exit 0 ;;
  gateway-stop) exit 0 ;;
  health) echo '{"ok":true,"ts":1,"sessions":{"count":0}}' ;;
  *) exit 99 ;;
esac
`)
	m := NewManager(Runner{Binary: bin}, time.Second, nil)

	ch1 := make(chan StepEvent, 8)
	RunAction(context.Background(), m, ch1, ActionStart)
	drainEvents(ch1)
	firstCount := len(m.ActionLog().Events)
	if firstCount == 0 {
		t.Fatalf("first action recorded zero events")
	}

	ch2 := make(chan StepEvent, 8)
	RunAction(context.Background(), m, ch2, ActionStop)
	drainEvents(ch2)

	entry := m.ActionLog()
	if entry.Name != "stop" {
		t.Fatalf("entry.Name: want stop got %q", entry.Name)
	}
	for _, ev := range entry.Events {
		if ev.Step == actionStepID(ActionStart) {
			t.Fatalf("stop log contains start event: %+v", ev)
		}
	}
}

// TestActionLogCapsAtMaxEvents guards the silent-drop policy when a
// pathological cascade emits more events than the buffer holds. The
// buffer must not grow unbounded and must keep the head (start
// events carry command context; losing them hurts more than losing
// the tail).
func TestActionLogCapsAtMaxEvents(t *testing.T) {
	m := NewManager(Runner{Binary: "/does-not-matter"}, time.Second, nil)
	m.setCurrentAction("fix")
	defer m.clearCurrentAction()
	for i := 0; i < ActionLogMaxEvents+50; i++ {
		m.RecordActionEvent(StepEvent{Step: StepDoctor, Phase: "start"})
	}
	entry := m.ActionLog()
	if len(entry.Events) != ActionLogMaxEvents {
		t.Fatalf("cap: want %d got %d", ActionLogMaxEvents, len(entry.Events))
	}
}

// TestActionLogSurvivesCurrentActionClear verifies the buffer is NOT
// wiped by clearCurrentAction — a refresh landing between the defer
// firing and the operator clicking again must still replay the last
// action. This is the subtle case the earlier drainEvents-based
// design got wrong.
func TestActionLogSurvivesCurrentActionClear(t *testing.T) {
	m := NewManager(Runner{Binary: "/does-not-matter"}, time.Second, nil)
	m.setCurrentAction("restart")
	m.RecordActionEvent(StepEvent{Step: actionStepID(ActionRestart), Phase: "start"})
	m.FinalizeActionLog("ok")
	m.clearCurrentAction()

	entry := m.ActionLog()
	if entry.Name != "restart" {
		t.Fatalf("name wiped after clearCurrentAction: %q", entry.Name)
	}
	if entry.Running {
		t.Fatalf("Running true after finalize")
	}
	if len(entry.Events) == 0 {
		t.Fatalf("events wiped after clearCurrentAction")
	}
}
