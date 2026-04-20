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

// handleOpenclawActionLog returns the most recent action's buffered
// StepEvents so a client that refreshed mid-stream can replay the CLI
// output it would otherwise have missed. Shape matches what the live
// NDJSON stream delivers: each element of `events` is a StepEvent the
// UI can pass straight to applyStreamEvent. `running` distinguishes
// "still going, keep polling /status" from "finished, paint outcome
// and move on".
//
// The entry persists until the NEXT action starts (setCurrentAction
// with a non-empty name resets the buffer), so a refresh landing
// after the action finished but before the user clicks again still
// sees the full transcript. When no action has ever run, `name` is
// empty and `events` is an empty array.
func (s *Server) handleOpenclawActionLog(w http.ResponseWriter, _ *http.Request) {
	if s.openclaw == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "openclaw integration disabled",
		})
		return
	}
	writeJSON(w, http.StatusOK, s.openclaw.ActionLog())
}

// handleOpenclawFix runs the Fix cascade and streams StepEvents as
// NDJSON (one JSON object per line). NDJSON is chosen over SSE so the
// browser client can use fetch+ReadableStream without an EventSource
// dance, and so curl operators can see raw output.
//
// Total cascade budget is the sum of per-tier timeouts plus
// probe-between-tiers overhead: ~30+5+15+5+10 = 65s. The action
// context is detached from r.Context() so a page refresh or tab close
// does not kill the cascade mid-tier — the Phase state machine and
// the Manager's currentAction/lastStartAttemptAt fields stay coherent
// for any subsequent client that polls /api/openclaw/status.
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

	// Detached from r.Context(): a disconnecting client must not
	// cancel the cascade. The 90s guardrail ensures a pathological
	// hang eventually frees the spawned CLI goroutine.
	//
	// cancel() is explicitly NOT deferred: returning from the handler
	// on client disconnect would then fire defer and kill the cascade
	// we just promised to detach. Each exit path owns the cleanup
	// (emitFinal calls cancel() after evCh closes; the clientDone
	// branch spawns a drain-then-cancel goroutine).
	actionCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)

	// Fix's intent is "make the gateway alive"; record it so a failure
	// to bring the gateway up renders as 异常 (not 已停止) in the UI.
	s.openclaw.SetIntent(openclaw.IntentRunning)

	evCh := make(chan openclaw.StepEvent, 8)
	go openclaw.Cascade(actionCtx, s.openclaw, evCh)

	clientDone := r.Context().Done()
	for {
		select {
		case ev, ok := <-evCh:
			if !ok {
				goto emitFinal
			}
			if err := enc.Encode(ev); err != nil {
				// Response broken but cascade still runs; drain evCh
				// so Cascade can complete and stamp
				// lastStartAttemptAt, then cancel() to release the
				// context. Cancelling eagerly would SIGKILL the CLI
				// mid-tier.
				go func() {
					drainEvents(evCh)
					cancel()
				}()
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case <-clientDone:
			// Browser refreshed / tab closed. Keep the cascade alive
			// so the gateway actually gets repaired; drain silently
			// and only then release the action context.
			go func() {
				drainEvents(evCh)
				cancel()
			}()
			return
		}
	}
emitFinal:
	// evCh closed: Cascade finished. Release the context now so the
	// 90s WithTimeout goroutine doesn't linger.
	cancel()
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

// drainEvents consumes every remaining StepEvent from evCh so that
// RunAction / Cascade never block on a send when the HTTP client
// disconnected mid-stream. The background action goroutine continues
// to completion and stamps currentAction / lastStartAttemptAt so the
// Phase state machine stays accurate after a page refresh.
func drainEvents(evCh <-chan openclaw.StepEvent) {
	for range evCh {
	}
}

// finalOutcome reports whether the Fix cascade revived the gateway.
// Unlike single actions (see actionFinalOutcome), the cascade's job
// is explicitly "make the gateway alive", so the post-cascade probe
// is the authoritative signal — the cascade already ran its own
// probes between tiers and any mid-cascade warm-up has had time to
// settle by the time this is called.
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
	//
	// Detached from r.Context(): the CLI invocation must survive a
	// browser refresh or tab close. If the action were tied to the
	// HTTP request, a refresh during restart would SIGKILL the
	// openclaw CLI mid-run — the Manager's lastStartAttemptAt would
	// never get stamped, so the Phase state machine would fall
	// through to "error" as soon as the warmup grace expired, even
	// though the user's intent was merely to reconnect the UI.
	//
	// cancel() is explicitly NOT deferred: returning from the handler
	// on client disconnect would then fire defer and kill the CLI we
	// just promised to detach. Instead, each exit path owns the
	// cleanup (emitFinal calls cancel() after the stream closes; the
	// clientDone branch spawns a drain-then-cancel goroutine).
	actionCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)

	// Record the user's stated intent before dispatching so the UI
	// badge can distinguish 已停止 (user stopped) from 异常 (crashed /
	// failed to come up). Doctor is read-only and leaves intent
	// unchanged so the previous intent survives a diagnose run.
	if intent := intentForAction(openclaw.Action(name)); intent != openclaw.IntentUnknown {
		s.openclaw.SetIntent(intent)
	}

	evCh := make(chan openclaw.StepEvent, 4)
	go openclaw.RunAction(actionCtx, s.openclaw, evCh, openclaw.Action(name))

	// cliOutcome captures the CLI step's own verdict so the terminal
	// "final" event reports it faithfully. Before this was derived from
	// a post-action probe, which raced with gateway warm-up and flipped
	// a successful restart to "重启失败" whenever the gateway took more
	// than a couple of seconds to answer health after coming up.
	var cliOutcome openclaw.StepOutcome
	clientDone := r.Context().Done()
	for {
		select {
		case ev, ok := <-evCh:
			if !ok {
				goto emitFinal
			}
			if ev.Phase == "end" && ev.Outcome != "" {
				cliOutcome = ev.Outcome
			}
			if err := enc.Encode(ev); err != nil {
				// Response broken but RunAction still runs; drain
				// silently so currentAction and lastStartAttemptAt
				// still get stamped. Same deferred-cancel pattern as
				// the clientDone branch: let the CLI finish before
				// tearing the action context down.
				go func() {
					drainEvents(evCh)
					cancel()
				}()
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case <-clientDone:
			// Browser refreshed / tab closed. Keep the CLI running
			// so the Phase state machine reports the real outcome
			// to whichever client polls /api/openclaw/status next.
			// cancel() is deferred until evCh drains so the action
			// context stays live for the full CLI lifetime; with an
			// eager cancel (or a naive defer cancel() on the handler),
			// the action context would be torn down the instant we
			// return, sending SIGKILL to the openclaw CLI and
			// recording a spurious outcome=failed / exit_code=-1 in
			// the ring buffer that any refreshed client would then
			// replay as "重启失败".
			go func() {
				drainEvents(evCh)
				cancel()
			}()
			return
		}
	}
emitFinal:
	// evCh closed: RunAction finished naturally. Release the context
	// now so the 45s WithTimeout goroutine doesn't linger.
	cancel()
	// Final event carries the fresh snapshot so the UI can update the
	// status card without a follow-up GET.
	final := struct {
		Step    string            `json:"step"`
		Phase   string            `json:"phase"`
		Outcome string            `json:"outcome"`
		Status  openclaw.Snapshot `json:"status"`
	}{
		Step:    "final",
		Phase:   "end",
		Outcome: actionFinalOutcome(cliOutcome),
		Status:  s.openclaw.Get(),
	}
	_ = enc.Encode(final)
	if flusher != nil {
		flusher.Flush()
	}
}

// actionFinalOutcome classifies a single CLI action's terminal result
// for the stream's "final" event. The CLI's own exit code is the
// authoritative signal — a successful `openclaw gateway restart`
// returning exit=0 with the expected stdout means the restart
// succeeded, even if the immediately-subsequent probe happens to miss
// because the freshly-started gateway has not finished warming up.
//
// Trusting the post-action probe over the CLI caused a recurring
// "console red / badge green" contradiction: the forced 5s post-probe
// window got killed by ProbeTimeout (12s), final event reported
// "failed", then the regular poller's next tick saw alive=true and
// flipped the badge to 运行中 — leaving the console line permanently
// lying about a restart that actually worked.
//
// Empty outcome (shouldn't happen — RunAction always emits an end
// event) is treated as success so we don't invent a failure.
func actionFinalOutcome(cliOutcome openclaw.StepOutcome) string {
	if cliOutcome == "" || cliOutcome == openclaw.OutcomeOK {
		return string(openclaw.OutcomeOK)
	}
	return string(cliOutcome)
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
