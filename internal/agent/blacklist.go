package agent

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/clawmast/clawmast/internal/blacklist"
	"github.com/clawmast/clawmast/internal/version"
)

// currentVersionLabel returns the version label the supervisor uses
// to refer to the running worker. The supervisor seeds it in
// CLAWMAST_VERSION_LABEL when spawning (internal/supervisor/session.go)
// and the worker must prefer it over version.Version so the blacklist
// entry matches the supervisor's pre-spawn check. A local `go run`
// without a supervisor falls back to the compile-time label.
func currentVersionLabel() string {
	if v := os.Getenv("CLAWMAST_VERSION_LABEL"); v != "" {
		return v
	}
	return version.Version
}

// BlacklistEntryDTO is the wire-shape for /api/blacklist entries. It
// mirrors blacklist.Entry one-to-one but renders the timestamp as
// RFC3339Nano string so JS consumers don't have to parse a Go time.
type BlacklistEntryDTO struct {
	Version   string `json:"version"`
	Timestamp string `json:"ts"`
	Reason    string `json:"reason,omitempty"`
}

// BlacklistResponse is the payload of GET /api/blacklist and the
// success payload of POST /api/blacklist.
type BlacklistResponse struct {
	Path    string              `json:"path"`
	Count   int                 `json:"count"`
	Entries []BlacklistEntryDTO `json:"entries"`
}

// BlacklistAddRequest is the POST body. Both fields are optional: an
// empty Version defaults to the worker's own version.Version so the
// UI "mark current as bad" path is a one-click POST with an empty
// body.
type BlacklistAddRequest struct {
	Version string `json:"version,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// BlacklistAddResponse extends BlacklistResponse with the flags the UI
// needs to show the right post-action state.
type BlacklistAddResponse struct {
	BlacklistResponse
	MarkedVersion     string `json:"marked_version"`
	Reason            string `json:"reason"`
	RollbackRequested bool   `json:"rollback_requested"`
}

// handleBlacklistList serves GET /api/blacklist. 503 is returned in
// standalone mode (no install root) so the UI knows to hide the
// related widget.
func (s *Server) handleBlacklistList(w http.ResponseWriter, _ *http.Request) {
	path, ok := s.blacklistPath(w)
	if !ok {
		return
	}
	entries, err := blacklist.Load(path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toBlacklistResponse(path, entries))
}

// handleBlacklistAdd serves POST /api/blacklist. When the request
// targets the currently-running version it writes the file *and*
// schedules an exit-65 via Config.RequestRollback so the supervisor
// rolls back to the previous slot (protocol §4 class Rollback). For
// any other version the file is updated but the worker keeps running
// — the block only takes effect the next time that version is
// selected for spawn.
func (s *Server) handleBlacklistAdd(w http.ResponseWriter, r *http.Request) {
	path, ok := s.blacklistPath(w)
	if !ok {
		return
	}
	var req BlacklistAddRequest
	if r.Body != nil {
		dec := json.NewDecoder(io.LimitReader(r.Body, 4*1024))
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	target := req.Version
	if target == "" {
		target = currentVersionLabel()
	}
	if target == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "no version to blacklist: request omitted version and worker has no version label",
		})
		return
	}
	reason := req.Reason
	if reason == "" {
		reason = "user-marked-bad"
	}
	entries, err := blacklist.Add(path, blacklist.Entry{Version: target, Reason: reason})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	rollback := target == currentVersionLabel() && s.cfg.RequestRollback != nil
	writeJSON(w, http.StatusAccepted, BlacklistAddResponse{
		BlacklistResponse: toBlacklistResponse(path, entries),
		MarkedVersion:     target,
		Reason:            reason,
		RollbackRequested: rollback,
	})
	if rollback {
		// Fire the rollback after a short delay so the HTTP response
		// has time to flush to the browser. The callback is non-
		// blocking; it signals the main run loop to return a
		// code-65 exit, which the supervisor classifies as Rollback.
		go func() {
			time.Sleep(150 * time.Millisecond)
			s.cfg.RequestRollback()
		}()
	}
}

// blacklistPath returns <InstallRoot>/state/blacklist.json and writes
// a 503 response when the server is running in standalone mode.
func (s *Server) blacklistPath(w http.ResponseWriter) (string, bool) {
	if s.cfg.InstallRoot == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "install root unknown; /api/blacklist is supervisor-scoped",
		})
		return "", false
	}
	return filepath.Join(s.cfg.InstallRoot, "state", "blacklist.json"), true
}

func toBlacklistResponse(path string, entries []blacklist.Entry) BlacklistResponse {
	dto := make([]BlacklistEntryDTO, 0, len(entries))
	for _, e := range entries {
		dto = append(dto, BlacklistEntryDTO{
			Version:   e.Version,
			Timestamp: e.Timestamp.UTC().Format(time.RFC3339Nano),
			Reason:    e.Reason,
		})
	}
	return BlacklistResponse{Path: path, Count: len(dto), Entries: dto}
}
