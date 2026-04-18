package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/clawmast/clawmast/internal/openclaw"
)

// handleOpenclawStatus serves the latest Snapshot produced by the
// background Poller. It never spawns the CLI inline — latency is < 1ms
// because the response body is a marshal of the in-memory struct.
//
// A missing Manager (Config.OpenClaw == nil) yields 503 so the UI can
// render "integration disabled" rather than an empty card.
func (s *Server) handleOpenclawStatus(w http.ResponseWriter, _ *http.Request) {
	if s.openclaw == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "openclaw integration disabled",
		})
		return
	}
	writeJSON(w, http.StatusOK, s.openclaw.Get())
}

// handleOpenclawFix runs the Fix cascade and streams StepEvents as
// NDJSON (one JSON object per line). NDJSON is chosen over SSE so the
// browser client can use fetch+ReadableStream without an EventSource
// dance, and so curl operators can see raw output.
//
// Total cascade budget is the sum of per-tier timeouts plus
// probe-between-tiers overhead: ~30+5+15+5+10 = 65s. The request
// context gets a 90s guardrail so a pathological hang eventually frees
// the connection.
func (s *Server) handleOpenclawFix(w http.ResponseWriter, r *http.Request) {
	if s.openclaw == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "openclaw integration disabled",
		})
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	// Disable buffering in common reverse proxies so each step lands
	// immediately instead of in a final chunk.
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	// Fix's intent is "make the gateway alive"; record it so a failure
	// to bring the gateway up renders as 异常 (not 已停止) in the UI.
	s.openclaw.SetIntent(openclaw.IntentRunning)

	evCh := make(chan openclaw.StepEvent, 8)
	go openclaw.Cascade(ctx, s.openclaw, evCh)

	for ev := range evCh {
		if err := enc.Encode(ev); err != nil {
			// Client disconnected or the response is broken; nothing
			// useful to do except stop the cascade and drain the chan.
			cancel()
			for range evCh {
			}
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	// Emit one terminal event with the post-cascade snapshot so the
	// UI can render the final state without a follow-up GET.
	final := struct {
		Step    string            `json:"step"`
		Phase   string            `json:"phase"`
		Outcome string            `json:"outcome"`
		Status  openclaw.Snapshot `json:"status"`
	}{
		Step:  "final",
		Phase: "end",
		// Fix's intent is "make the gateway alive"; a post-cascade probe
		// that still reports !alive means the cascade failed to repair.
		Outcome: finalOutcome(s.openclaw.Get().Alive, true),
		Status:  s.openclaw.Get(),
	}
	_ = enc.Encode(final)
	if flusher != nil {
		flusher.Flush()
	}
}

// finalOutcome reports whether the post-action probe matches the
// caller's intent. start / restart / fix expect alive=true; stop
// expects alive=false; doctor has no expectation and passes
// expectedAlive = actual so the outcome is always "ok".
//
// The previous implementation inverted the stop case — it reported
// "failed" whenever !alive, so a successful stop showed as "停止失败"
// in the UI. Intent-aware evaluation fixes that without the UI having
// to special-case the action name.
func finalOutcome(alive, expectedAlive bool) string {
	if alive == expectedAlive {
		return "ok"
	}
	return "failed"
}

// handleOpenclawAction streams a single openclaw CLI invocation
// (start / stop / restart / doctor) as NDJSON. Shape matches the Fix
// cascade (one start event + one end event with command output tails)
// so the UI can reuse the same stream-panel renderer for both.
//
// The ?name= query parameter picks the action; openclaw.ValidAction
// gatekeeps it so a typo returns 400 instead of spawning a spurious
// openclaw subcommand.
func (s *Server) handleOpenclawAction(w http.ResponseWriter, r *http.Request) {
	if s.openclaw == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "openclaw integration disabled",
		})
		return
	}
	name := r.URL.Query().Get("name")
	if !openclaw.ValidAction(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "unknown action; expected one of start, stop, restart, doctor",
		})
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)

	// 45s is the conservative upper bound: 30s doctor + 5s post-probe +
	// a margin for slow Node startup. All four actions fit within this.
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	// Record the user's stated intent before dispatching so the UI
	// badge can distinguish 已停止 (user stopped) from 异常 (crashed /
	// failed to come up). Doctor is read-only and leaves intent
	// unchanged so the previous intent survives a diagnose run.
	if intent := intentForAction(openclaw.Action(name)); intent != openclaw.IntentUnknown {
		s.openclaw.SetIntent(intent)
	}

	evCh := make(chan openclaw.StepEvent, 4)
	go openclaw.RunAction(ctx, s.openclaw, evCh, openclaw.Action(name))

	for ev := range evCh {
		if err := enc.Encode(ev); err != nil {
			cancel()
			for range evCh {
			}
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	// Final event carries the fresh snapshot so the UI can update the
	// status card without a follow-up GET.
	final := struct {
		Step    string            `json:"step"`
		Phase   string            `json:"phase"`
		Outcome string            `json:"outcome"`
		Status  openclaw.Snapshot `json:"status"`
	}{
		Step:  "final",
		Phase: "end",
		// Each action carries an expectation about the post-probe alive
		// state. Matching expectation -> "ok"; mismatch -> "failed".
		// Doctor is read-only so its expectation is "unchanged", which
		// we approximate by passing alive for both args (always "ok").
		Outcome: finalOutcome(s.openclaw.Get().Alive, expectedAliveAfter(openclaw.Action(name), s.openclaw.Get().Alive)),
		Status:  s.openclaw.Get(),
	}
	_ = enc.Encode(final)
	if flusher != nil {
		flusher.Flush()
	}
}

// expectedAliveAfter returns the alive state each action intends to
// leave the gateway in. Doctor is read-only; we pass through the
// observed alive so finalOutcome always reports "ok".
func expectedAliveAfter(a openclaw.Action, observedAlive bool) bool {
	switch a {
	case openclaw.ActionStop:
		return false
	case openclaw.ActionStart, openclaw.ActionRestart:
		return true
	case openclaw.ActionDoctor:
		return observedAlive
	}
	return observedAlive
}

// intentForAction maps an action to the operator intent it encodes.
// Returned value is stored on the Snapshot so the UI can pick the
// right badge (已停止 vs 异常) when the next probe reports !alive.
// Doctor returns IntentUnknown — it is read-only and must not
// clobber the previous intent.
func intentForAction(a openclaw.Action) openclaw.Intent {
	switch a {
	case openclaw.ActionStop:
		return openclaw.IntentStopped
	case openclaw.ActionStart, openclaw.ActionRestart:
		return openclaw.IntentRunning
	}
	return openclaw.IntentUnknown
}
