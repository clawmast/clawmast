package agent

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/clawmast/clawmast/internal/history"
	"github.com/clawmast/clawmast/internal/updater"
	"github.com/clawmast/clawmast/internal/version"
)

// VersionResponse mirrors the payload of GET /api/version. Fields are
// deliberately flat and snake_case so the JS client can bind to them
// without a schema layer.
type VersionResponse struct {
	// Version is the ldflags-injected semver-ish string from
	// internal/version. For local dev builds this is a git-describe
	// fallback like "abc123-dirty".
	Version string `json:"version"`
	// Label is the supervisor's notion of the running version —
	// the directory name behind the "current" symlink, propagated
	// via CLAWMAST_VERSION_LABEL. The UI prefers this when showing
	// "current version" so the label matches what the installer and
	// blacklist use. Equals Version under standalone mode.
	Label     string `json:"label"`
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

// UpdateCheckResponse is the payload of POST /api/updates/check.
// Iteration 2 (architecture/refactor.md §6) replaced the stub with a
// real signed-manifest round-trip; the "source" field distinguishes
// the outcomes the UI needs to render differently:
//
//   - "signed-manifest": Current / Latest carry real data and the
//     signature verified against the embedded dev pubkey.
//   - "not-configured": the worker has no CLAWMAST_UPDATE_URL set, so
//     we report the running version as both current and latest without
//     pretending we reached a channel.
//   - "error": the channel round-trip failed; "note" carries the
//     reason and "error_code" categorises it (bad-signature,
//     channel-mismatch, manifest-missing, unreachable).
type UpdateCheckResponse struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
	Channel         string `json:"channel"`
	Source          string `json:"source"`
	PublishedAt     string `json:"published_at,omitempty"`
	Notes           string `json:"notes,omitempty"`
	ErrorCode       string `json:"error_code,omitempty"`
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
		Label:     currentVersionLabel(),
		Commit:    version.Commit,
		BuildTime: version.BuildTime,
		GoVersion: goVersion,
		Full:      version.Full(),
	})
}

// handleUpdateCheck performs the Iteration 2 signed-manifest round
// trip: fetch <UpdateBaseURL>/manifest.json + .minisig, verify the
// signature, and compare the advertised version to the running one.
// Shape is UpdateCheckResponse; see that doc comment for the three
// possible "source" values (signed-manifest / not-configured / error).
//
// The request carries no body today; POST was chosen during Iteration 0
// to leave room for channel / force flags without breaking a cached GET.
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	current := currentVersionLabel()
	if s.updater == nil {
		writeJSON(w, http.StatusOK, UpdateCheckResponse{
			Current:         current,
			Latest:          current,
			UpdateAvailable: false,
			Channel:         s.cfg.UpdateChannel,
			Source:          "not-configured",
			Note:            "CLAWMAST_UPDATE_URL is unset; set it to the channel base URL (for example https://update.clawmast.com/stable) to enable update checks.",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	manifest, err := s.updater.Check(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, UpdateCheckResponse{
			Current:         current,
			Latest:          current,
			UpdateAvailable: false,
			Channel:         s.cfg.UpdateChannel,
			Source:          "error",
			ErrorCode:       classifyUpdaterError(err),
			Note:            err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, UpdateCheckResponse{
		Current:         current,
		Latest:          manifest.Version,
		UpdateAvailable: manifest.Version != current,
		Channel:         manifest.Channel,
		Source:          "signed-manifest",
		PublishedAt:     manifest.PublishedAt.UTC().Format(time.RFC3339),
		Notes:           manifest.Notes,
	})
}

// classifyUpdaterError maps the updater package's sentinel errors to
// stable machine-readable codes so the UI can render targeted copy
// ("signature failed — do not install this binary" vs "channel is
// unreachable — try again later") without string-matching error text.
func classifyUpdaterError(err error) string {
	switch {
	case errors.Is(err, updater.ErrBadSignature):
		return "bad-signature"
	case errors.Is(err, updater.ErrChannelMismatch):
		return "channel-mismatch"
	case errors.Is(err, updater.ErrManifestMissing):
		return "manifest-missing"
	case errors.Is(err, updater.ErrManifestTooBig):
		return "manifest-too-big"
	case errors.Is(err, updater.ErrBadURL):
		return "bad-url"
	default:
		return "unreachable"
	}
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
