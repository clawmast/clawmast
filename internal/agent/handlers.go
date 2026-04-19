package agent

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/clawmast/clawmast/internal/history"
	"github.com/clawmast/clawmast/internal/selfupdate"
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
	// SupervisorVersion is the clawmastd build string propagated via
	// CLAWMAST_SUPERVISOR_VERSION at spawn. Empty when the worker runs
	// standalone (no supervisor in the loop); the UI renders this as
	// "监工 —" so the operator can tell supervised from bare runs.
	SupervisorVersion string `json:"supervisor_version,omitempty"`
	// InstallRoot is the <root> the worker is resolving supervisor-
	// scoped state against. Empty in standalone mode. Exposed so the
	// dashboard can show a single "系统" card without a second endpoint.
	InstallRoot string `json:"install_root,omitempty"`
	// Channel is the effective update channel for this worker boot
	// (preference file > env > default). Mirrors /api/updates/check's
	// Channel field; duplicated here so the UI can render the system
	// card without forcing an updater round-trip.
	Channel string `json:"channel,omitempty"`
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
		Version:           version.Version,
		Label:             currentVersionLabel(),
		Commit:            version.Commit,
		BuildTime:         version.BuildTime,
		GoVersion:         goVersion,
		Full:              version.Full(),
		SupervisorVersion: s.cfg.SupervisorVersion,
		InstallRoot:       s.cfg.InstallRoot,
		Channel:           s.cfg.UpdateChannel,
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

// UpdateInstallResponse is the payload of POST /api/updates/install.
// On success it mirrors the on-disk outcome of [selfupdate.Apply] so
// the UI can render "now on v1.2.3, was v1.2.2" without a follow-up
// /api/version poll. The handler fires [Config.RequestRestart] *after*
// serialising this response so the client sees the final state before
// the worker exits to let the supervisor respawn.
//
// error_code values mirror handleUpdateCheck where they overlap
// (bad-signature, channel-mismatch, manifest-missing, ...) and add the
// install-specific ones from [selfupdate]:
//
//   - "bad-sha256"         artifact hash mismatch — signed manifest says
//     the bytes on the CDN are not what was signed; do not retry until
//     the operator understands why.
//   - "size-mismatch"      artifact size differs from the manifest.
//   - "bad-tarball"        archive is malformed or contains unsafe paths.
//   - "no-artifact"        manifest carries nothing for this host.
//   - "already-installed"  running version matches the manifest; no-op.
//   - "not-configured"     CLAWMAST_UPDATE_URL unset.
//   - "install-root-unknown"  running standalone without clawmastd.
//   - "restart-unavailable"   no RequestRestart hook wired.
type UpdateInstallResponse struct {
	OK               bool   `json:"ok"`
	Version          string `json:"version,omitempty"`
	PreviousVersion  string `json:"previous_version,omitempty"`
	CurrentAfter     string `json:"current_after,omitempty"`
	PreviousAfter    string `json:"previous_after,omitempty"`
	BytesDownloaded  int64  `json:"bytes_downloaded,omitempty"`
	RestartRequested bool   `json:"restart_requested,omitempty"`
	ErrorCode        string `json:"error_code,omitempty"`
	Note             string `json:"note,omitempty"`
}

// handleUpdateInstall runs the download → verify → rotate pipeline and
// fires RequestRestart on success. The restart is scheduled on a short
// timer so the HTTP response flushes before the worker exits; without
// the delay, the client often sees a TCP RST before reading the body.
//
// Guardrails in order:
//  1. s.updater must be configured (CLAWMAST_UPDATE_URL). Otherwise
//     we have nowhere to fetch the manifest from.
//  2. s.cfg.InstallRoot must be known. In standalone mode there is no
//     versions/ tree to rotate, so install is meaningless.
//  3. s.cfg.RequestRestart must be wired. Without it we would leave the
//     worker running the old binary with the new `current` symlink,
//     which is visually confusing and undermines the supervisor
//     respawn guarantee.
func (s *Server) handleUpdateInstall(w http.ResponseWriter, r *http.Request) {
	if s.updater == nil {
		writeJSON(w, http.StatusServiceUnavailable, UpdateInstallResponse{
			ErrorCode: "not-configured",
			Note:      "CLAWMAST_UPDATE_URL is unset; set it to the channel base URL (for example https://update.clawmast.com/stable) to enable auto-update.",
		})
		return
	}
	if s.cfg.InstallRoot == "" {
		writeJSON(w, http.StatusServiceUnavailable, UpdateInstallResponse{
			ErrorCode: "install-root-unknown",
			Note:      "install root unknown; /api/updates/install is supervisor-scoped",
		})
		return
	}
	if s.cfg.RequestRestart == nil {
		writeJSON(w, http.StatusServiceUnavailable, UpdateInstallResponse{
			ErrorCode: "restart-unavailable",
			Note:      "worker was started without a restart hook; refuse to rotate current without a path back to a running version",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	manifest, err := s.updater.Check(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, UpdateInstallResponse{
			ErrorCode: classifyUpdaterError(err),
			Note:      err.Error(),
		})
		return
	}

	result, err := selfupdate.Apply(ctx, selfupdate.Config{
		Manifest:       manifest,
		InstallRoot:    s.cfg.InstallRoot,
		CurrentVersion: currentVersionLabel(),
	})
	if err != nil {
		code, status := classifySelfupdateError(err)
		writeJSON(w, status, UpdateInstallResponse{
			ErrorCode: code,
			Note:      err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, UpdateInstallResponse{
		OK:               true,
		Version:          result.Version,
		PreviousVersion:  currentVersionLabel(),
		CurrentAfter:     result.CurrentAfter,
		PreviousAfter:    result.PreviousAfter,
		BytesDownloaded:  result.BytesDownloaded,
		RestartRequested: true,
	})
	// Give the kernel a moment to flush the response before the
	// supervisor tears down the TCP listener. 250 ms is well inside
	// the UI polling loop and well outside the usual flush latency.
	time.AfterFunc(250*time.Millisecond, s.cfg.RequestRestart)
}

// classifySelfupdateError maps [selfupdate]'s sentinels onto the same
// error_code string space as classifyUpdaterError plus install-specific
// codes. Returns the HTTP status to use; integrity failures are 502
// (the CDN gave us bytes that disagree with the signed manifest) while
// "no artifact" / "already installed" are 409 (request makes sense but
// does not describe a state that can be acted upon).
func classifySelfupdateError(err error) (code string, status int) {
	switch {
	case errors.Is(err, selfupdate.ErrBadSHA256):
		return "bad-sha256", http.StatusBadGateway
	case errors.Is(err, selfupdate.ErrSizeMismatch):
		return "size-mismatch", http.StatusBadGateway
	case errors.Is(err, selfupdate.ErrBadTarball):
		return "bad-tarball", http.StatusBadGateway
	case errors.Is(err, selfupdate.ErrNoArtifactForHost):
		return "no-artifact", http.StatusConflict
	case errors.Is(err, selfupdate.ErrAlreadyInstalled):
		return "already-installed", http.StatusConflict
	default:
		return "install-failed", http.StatusInternalServerError
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
