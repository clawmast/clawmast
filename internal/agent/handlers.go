package agent

import (
	"net/http"
	"time"

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
