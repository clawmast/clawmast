package agent

import (
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/clawmast/clawmast/internal/history"
	"github.com/clawmast/clawmast/internal/version"
)

// VersionResponse mirrors the payload of GET /api/version. Fields are
// deliberately flat and snake_case so the JS client can bind to them
// without a schema layer.
type VersionResponse struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
	Full      string `json:"full"`
}

// HealthResponse is the payload of GET /api/health. The embedded UI
// polls this while the worker boots; the simplest useful shape is a
// boolean + an uptime hint.
type HealthResponse struct {
	OK        bool   `json:"ok"`
	Service   string `json:"service"`
	UptimeMS  int64  `json:"uptime_ms"`
	Timestamp string `json:"timestamp"`
}

// UpdateCheckResponse is the Iteration 0 stub for POST
// /api/updates/check. Real channel and signature logic ships in
// Iteration 2 per architecture/refactor.md §6; for now the endpoint
// reports the running version as "latest" so the UI round-trip works
// end-to-end without pretending an update is ready.
type UpdateCheckResponse struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
	Channel         string `json:"channel"`
	Source          string `json:"source"`
	Note            string `json:"note,omitempty"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().UTC()
	writeJSON(w, http.StatusOK, HealthResponse{
		OK:        true,
		Service:   "clawmast",
		UptimeMS:  now.Sub(s.started).Milliseconds(),
		Timestamp: now.Format(time.RFC3339Nano),
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, VersionResponse{
		Version:   version.Version,
		Commit:    version.Commit,
		BuildTime: version.BuildTime,
		GoVersion: goVersion,
		Full:      version.Full(),
	})
}

// handleUpdateCheck implements the Iteration 0 stub for the "check for
// updates" button. It returns 200 with update_available=false and a
// human-readable note so the UI can display the behaviour truthfully
// without a placeholder "coming soon" pop-up.
func (s *Server) handleUpdateCheck(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, UpdateCheckResponse{
		Current:         version.Version,
		Latest:          version.Version,
		UpdateAvailable: false,
		Channel:         "stable",
		Source:          "stub",
		Note:            "update channel lands in Iteration 2 (architecture/refactor.md §6); this endpoint is a placeholder so the UI round-trip works end-to-end.",
	})
}

// HistoryEntryDTO is the wire-shape for /api/history. It mirrors the
// on-disk ledger (history.Entry) one-to-one but exposes the exit code
// as int64 with an explicit "has_exit" flag so JS consumers don't have
// to special-case null vs 0.
type HistoryEntryDTO struct {
	Timestamp string `json:"ts"`
	Event     string `json:"event"`
	Version   string `json:"version,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// HistoryResponse is the payload of GET /api/history.
type HistoryResponse struct {
	Path    string            `json:"path"`
	Count   int               `json:"count"`
	Entries []HistoryEntryDTO `json:"entries"`
}

// handleHistory serves the supervisor-written history ledger. Iteration
// 1 (architecture/refactor.md §9: "Upgrade history & visibility") uses
// it to render the timeline in the worker UI.
//
// Query: ?limit=N (default 50, cap 500) tails the newest N entries.
// 503 is returned when the worker does not know where the install root
// is — for example when running via `go run ./cmd/clawmast` without
// clawmastd — so consumers can detect "standalone mode" without a
// separate capability check.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if s.cfg.InstallRoot == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "install root unknown; /api/history is supervisor-scoped",
		})
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			if n > 500 {
				n = 500
			}
			limit = n
		}
	}
	path := filepath.Join(s.cfg.InstallRoot, "state", "history.json")
	entries, err := history.ReadTail(path, history.ReadOptions{Limit: limit})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
		return
	}
	dto := make([]HistoryEntryDTO, 0, len(entries))
	for _, e := range entries {
		dto = append(dto, HistoryEntryDTO{
			Timestamp: e.Timestamp.UTC().Format(time.RFC3339Nano),
			Event:     string(e.Event),
			Version:   e.Version,
			ExitCode:  e.ExitCode,
			Reason:    e.Reason,
		})
	}
	writeJSON(w, http.StatusOK, HistoryResponse{
		Path: path, Count: len(dto), Entries: dto,
	})
}
