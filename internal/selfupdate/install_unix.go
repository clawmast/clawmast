//go:build unix

package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/clawmast/clawmast/internal/updater"
)

// Apply installs the artifact that matches the current host from a
// signature-verified manifest. On success it returns a Result
// describing the new on-disk layout; the caller is expected to signal
// the supervisor (via a clean worker exit) so the freshly-rotated
// `current` link gets picked up on the next spawn.
//
// Apply is safe to interrupt via ctx: it writes to a version directory
// under a temporary name and only renames into place after the SHA-256
// check passes and the tarball is fully extracted. A failure before
// the rename leaves <versions/>.<version>.partial/ for manual
// inspection; it is never referenced by the supervisor.
func Apply(ctx context.Context, cfg Config) (Result, error) {
	if cfg.Manifest == nil {
		return Result{}, fmt.Errorf("selfupdate: nil manifest")
	}
	if cfg.InstallRoot == "" {
		return Result{}, fmt.Errorf("selfupdate: empty install root")
	}
	if err := cfg.Manifest.Validate(); err != nil {
		return Result{}, fmt.Errorf("selfupdate: %w", err)
	}

	art, ok := cfg.Manifest.ForCurrentHost()
	if !ok {
		return Result{}, fmt.Errorf("%w: %s/%s", ErrNoArtifactForHost, runtime.GOOS, runtime.GOARCH)
	}
	if cfg.CurrentVersion != "" && cfg.CurrentVersion == cfg.Manifest.Version {
		return Result{}, ErrAlreadyInstalled
	}

	client := cfg.HTTPClient
	if client == nil {
		// The default here errs on the generous side; artifact
		// downloads are expected to be ≤ tens of MB over best-effort
		// residential links. Per-request deadlines via ctx are the
		// real timeout knob.
		client = &http.Client{Timeout: 10 * time.Minute}
	}

	version := cfg.Manifest.Version
	versionsDir := filepath.Join(cfg.InstallRoot, "versions")
	if err := os.MkdirAll(versionsDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("selfupdate: mkdir versions: %w", err)
	}
	// Extract into a sibling .partial directory first so a crash mid-
	// extract leaves the real versions/<version>/ untouched. On
	// success we rename the partial into place atomically.
	target := filepath.Join(versionsDir, version)
	staging := filepath.Join(versionsDir, "."+version+".partial")
	if err := os.RemoveAll(staging); err != nil {
		return Result{}, fmt.Errorf("selfupdate: clear staging: %w", err)
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return Result{}, fmt.Errorf("selfupdate: mkdir staging: %w", err)
	}

	n, err := downloadAndExtract(ctx, client, art, staging)
	if err != nil {
		_ = os.RemoveAll(staging)
		return Result{}, err
	}
	if err := writeVersionManifest(staging, version, art); err != nil {
		_ = os.RemoveAll(staging)
		return Result{}, fmt.Errorf("selfupdate: write MANIFEST: %w", err)
	}

	// Final rename brings the version directory into place. If target
	// already exists (e.g. a previous interrupted install left debris)
	// clear it; the staging dir we just built is the source of truth.
	if _, err := os.Stat(target); err == nil {
		if err := os.RemoveAll(target); err != nil {
			return Result{}, fmt.Errorf("selfupdate: clear stale target: %w", err)
		}
	}
	if err := os.Rename(staging, target); err != nil {
		return Result{}, fmt.Errorf("selfupdate: promote staging: %w", err)
	}

	before, after, prev, err := rotateSymlinks(cfg.InstallRoot, version)
	if err != nil {
		return Result{}, fmt.Errorf("selfupdate: rotate symlinks: %w", err)
	}

	return Result{
		Version:         version,
		VersionDir:      target,
		CurrentBefore:   before,
		CurrentAfter:    after,
		PreviousAfter:   prev,
		BytesDownloaded: n,
	}, nil
}

// writeVersionManifest lays down the per-install MANIFEST.json with
// the same shape install.sh produces. Keeping the two paths consistent
// means readers of the install tree (operators, future selfcheck
// tooling) see the same invariants regardless of install source.
func writeVersionManifest(dir, version string, art updater.Artifact) error {
	payload := map[string]any{
		"version":      version,
		"os":           art.OS,
		"arch":         art.Arch,
		"sha256":       art.SHA256,
		"size":         art.Size,
		"installed_at": time.Now().UTC().Format(time.RFC3339),
		"source":       "auto-update",
	}
	buf, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "MANIFEST.json"), append(buf, '\n'), 0o600)
}
