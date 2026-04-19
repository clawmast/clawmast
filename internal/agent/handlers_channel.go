package agent

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// ChannelResponse is the payload of GET /api/settings/channel. It
// carries the *active* channel (what /api/updates/check will use on
// the next call) plus the channel the user most recently persisted.
// When the two disagree the UI renders a "切换将在重启后生效" hint so
// the operator is not surprised by a stale-looking display.
type ChannelResponse struct {
	// Active is the channel this running worker resolved at boot
	// (preference-file > env > default).
	Active string `json:"active"`
	// Preference is the contents of <state>/channel. Empty when the
	// user has not yet made an explicit pick via the UI.
	Preference string `json:"preference,omitempty"`
	// Available enumerates the channels the UI is allowed to offer.
	// Hard-coded to stable/beta in I3; the refactor keeps this field
	// so later iterations can advertise "internal" without an API bump.
	Available []string `json:"available"`
	// RestartRequired is true when Preference != Active, meaning the
	// worker still needs a restart to pick up the user's pick. The UI
	// offers a "立即重启" button in that case.
	RestartRequired bool `json:"restart_required"`
}

// ChannelSetRequest is the POST body for /api/settings/channel.
type ChannelSetRequest struct {
	Channel string `json:"channel"`
	// Restart, when true, asks the worker to exit 0 after persisting
	// so the supervisor respawns and picks up the new channel without
	// the operator doing it by hand. The worker still flushes the HTTP
	// response first (same deferred-restart trick as updates/install).
	Restart bool `json:"restart,omitempty"`
}

// ChannelSetResponse mirrors the ChannelResponse shape after a write
// plus a RestartRequested flag so the UI knows whether to wait for
// supervisor respawn or simply update its view.
type ChannelSetResponse struct {
	Active           string   `json:"active"`
	Preference       string   `json:"preference"`
	Available        []string `json:"available"`
	RestartRequired  bool     `json:"restart_required"`
	RestartRequested bool     `json:"restart_requested"`
}

// handleChannelGet returns the current channel preference. Available
// in standalone mode (returns Preference="" when StateDir is unset)
// because the UI needs a GET to render the picker even before the
// user writes a preference.
func (s *Server) handleChannelGet(w http.ResponseWriter, _ *http.Request) {
	active := s.cfg.UpdateChannel
	if active == "" {
		active = ChannelStable
	}
	pref := ""
	if s.cfg.StateDir != "" {
		if p, err := ReadChannelPreference(s.cfg.StateDir); err == nil {
			pref = p
		}
	}
	writeJSON(w, http.StatusOK, ChannelResponse{
		Active:          active,
		Preference:      pref,
		Available:       []string{ChannelStable, ChannelBeta},
		RestartRequired: pref != "" && pref != active,
	})
}

// handleChannelSet persists the channel preference and optionally
// asks the worker to restart so the supervisor respawns against the
// new channel. The write goes through WriteChannelPreference's atomic
// rename so an interrupted POST cannot leave a half-written file.
func (s *Server) handleChannelSet(w http.ResponseWriter, r *http.Request) {
	if s.cfg.StateDir == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "state dir unknown; channel preference requires supervisor-managed state",
		})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var req ChannelSetRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
		return
	}
	pref, err := WriteChannelPreference(s.cfg.StateDir, req.Channel)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrUnknownChannel) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}

	active := s.cfg.UpdateChannel
	if active == "" {
		active = ChannelStable
	}
	restartRequired := pref != active
	restartRequested := false
	if req.Restart && restartRequired && s.cfg.RequestRestart != nil {
		restartRequested = true
	}

	writeJSON(w, http.StatusOK, ChannelSetResponse{
		Active:           active,
		Preference:       pref,
		Available:        []string{ChannelStable, ChannelBeta},
		RestartRequired:  restartRequired,
		RestartRequested: restartRequested,
	})

	if restartRequested {
		// Mirror updates/install: flush the response, then ask the
		// supervisor to respawn us so the new channel takes effect.
		time.AfterFunc(250*time.Millisecond, s.cfg.RequestRestart)
	}
}
