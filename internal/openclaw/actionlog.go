package openclaw

import "time"

// ActionLogMaxEvents caps the per-action event buffer. A normal
// start/stop/restart/doctor emits 2–3 events; a full Fix cascade emits
// ~8. Sizing at 256 leaves plenty of headroom for future step types
// while bounding memory to a few KB per Manager. Beyond the cap we
// silently drop the tail — the overflow case is pathological and the
// head (start events, which carry command context) is the more
// useful survivor.
const ActionLogMaxEvents = 256

// ActionLogEntry is the persistent record of one user-initiated action
// (start / stop / restart / doctor / fix). The Manager keeps exactly
// one slot — the most recent action — so a client that refreshes in
// the middle of a long-running command can replay the CLI output
// emitted while its JS context was being rebuilt. The slot is reset
// when a new action starts (setCurrentAction with a non-empty name).
//
// Name is the wire-identical action verb ("start" / "restart" /
// "fix" / ...). Running is true while the action is still executing;
// the handlers call FinalizeActionLog at the end to flip it false
// and stamp FinishedAt + Outcome.
type ActionLogEntry struct {
	Name       string      `json:"name"`
	Running    bool        `json:"running"`
	Outcome    string      `json:"outcome,omitempty"`
	StartedAt  time.Time   `json:"started_at"`
	FinishedAt time.Time   `json:"finished_at,omitempty"`
	Events     []StepEvent `json:"events"`
}

// resetActionLog clears the buffer and stamps a fresh StartedAt so the
// next recordActionEvent writes into an empty slot. Called from
// setCurrentAction when a new action is dispatched.
func (m *Manager) resetActionLog(name string) {
	m.actionLogMu.Lock()
	defer m.actionLogMu.Unlock()
	m.actionLog = ActionLogEntry{
		Name:      name,
		Running:   true,
		StartedAt: time.Now().UTC(),
		Events:    make([]StepEvent, 0, 8),
	}
}

// RecordActionEvent appends ev to the current action's log buffer.
// RunAction and Cascade call this right before sending ev on the live
// channel so a client that disconnects mid-stream (page refresh) can
// still replay the events that landed after the disconnect. Silently
// no-ops when the log is idle (Name is "") or at capacity — the live
// channel is the primary delivery path and must not block on log
// bookkeeping.
func (m *Manager) RecordActionEvent(ev StepEvent) {
	m.actionLogMu.Lock()
	defer m.actionLogMu.Unlock()
	if m.actionLog.Name == "" {
		return
	}
	if len(m.actionLog.Events) >= ActionLogMaxEvents {
		return
	}
	m.actionLog.Events = append(m.actionLog.Events, ev)
}

// FinalizeActionLog stamps FinishedAt and Outcome so a replay client
// can paint the completion state (ok → 完成, anything else → 失败).
// Called by RunAction / Cascade after the final event has been
// emitted on the live channel. Leaves Name and Events intact so a
// late refresh (arriving after the action finished but before the
// next action starts) still sees the full transcript.
func (m *Manager) FinalizeActionLog(outcome string) {
	m.actionLogMu.Lock()
	defer m.actionLogMu.Unlock()
	if m.actionLog.Name == "" {
		return
	}
	m.actionLog.Running = false
	m.actionLog.Outcome = outcome
	m.actionLog.FinishedAt = time.Now().UTC()
}

// ActionLog returns a defensive copy of the current log entry. The
// /api/openclaw/action/log handler calls this on each request;
// returning a copy means the HTTP layer cannot race with ongoing
// RecordActionEvent writes even if encoding is slow.
func (m *Manager) ActionLog() ActionLogEntry {
	m.actionLogMu.Lock()
	defer m.actionLogMu.Unlock()
	out := m.actionLog
	if len(m.actionLog.Events) > 0 {
		out.Events = make([]StepEvent, len(m.actionLog.Events))
		copy(out.Events, m.actionLog.Events)
	} else {
		out.Events = []StepEvent{}
	}
	return out
}
