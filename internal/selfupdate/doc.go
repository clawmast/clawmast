// Package selfupdate installs a newer worker version described by a
// signed update manifest (see [updater.Manifest]). It is the companion
// to [updater], which only fetches and verifies manifests. This package
// performs the side-effectful half: download the release tarball,
// verify its SHA-256, extract it under <install-root>/versions/<ver>/,
// rotate the current/previous symlinks, and write the per-install
// MANIFEST.json that install.sh lays down for shell-based installs.
//
// The caller is expected to have already fetched a verified manifest
// via updater.Client. Passing an unverified manifest to [Apply] is a
// programming error: signature enforcement is a non-negotiable
// prerequisite per refactor.md §6, and this package does not re-verify.
//
// # Restart semantics
//
// [Apply] only rotates symlinks; it does not terminate the worker. The
// caller is responsible for triggering a clean shutdown afterwards so
// clawmastd respawns against the freshly-rotated `current` link. The
// worker exits cleanly (code 0) and the supervisor demotes the
// spontaneous-graceful-during-Running to ClassUnexpected per
// supervisor-protocol.md §4 edge rules, which is exactly the "respawn
// against new binary" behaviour we want.
//
// # Windows
//
// Auto-update is a Tier-1 (macOS / Linux) feature per refactor.md
// decision #2. Windows builds link against [Apply] via an
// [errors.ErrUnsupported] stub so the worker binary remains
// windows/amd64-compilable; manual install stays the supported path on
// Windows until Tier-1 parity lands post-v1.0.
package selfupdate

import (
	"errors"
	"net/http"

	"github.com/clawmast/clawmast/internal/updater"
)

// Config controls one invocation of [Apply].
type Config struct {
	// Manifest is the already-verified manifest describing the release
	// to install. The artifact matching runtime.GOOS/GOARCH is
	// installed; [Apply] fails with ErrNoArtifactForHost otherwise.
	Manifest *updater.Manifest

	// InstallRoot is the clawmastd install tree, the one that owns
	// versions/, current, previous, state/. [Apply] writes under
	// versions/<version>/ and rotates the symlinks in this directory.
	InstallRoot string

	// HTTPClient is used to download the artifact. Nil means the
	// package picks an http.Client with a reasonable timeout.
	HTTPClient *http.Client

	// CurrentVersion is the version label of the worker running
	// Apply. Used only to short-circuit with ErrAlreadyInstalled when
	// the manifest advertises the same version; a mismatch never
	// prevents installation (downgrades are allowed).
	CurrentVersion string
}

// Result describes a successful install. Fields mirror the fields the
// MANIFEST.json carries, so callers can log the same shape the
// installer writes.
type Result struct {
	// Version is the version label installed, e.g. "v1.2.3". Mirrors
	// Manifest.Version.
	Version string
	// VersionDir is the absolute path of the newly-installed version
	// directory, i.e. <InstallRoot>/versions/<Version>/.
	VersionDir string
	// CurrentBefore is the target of the `current` symlink before
	// rotation. Empty string if there was no prior install.
	CurrentBefore string
	// CurrentAfter is the target of `current` after rotation, always
	// "versions/<Version>".
	CurrentAfter string
	// PreviousAfter is the target of `previous` after rotation. Empty
	// when there was no prior install to promote.
	PreviousAfter string
	// BytesDownloaded is the exact number of bytes fetched from the
	// artifact URL. Matches Manifest.Artifact.Size on success.
	BytesDownloaded int64
}

// Errors surfaced by Apply. Callers compare with [errors.Is].
var (
	// ErrNoArtifactForHost is returned when the manifest has no
	// artifact for runtime.GOOS/runtime.GOARCH. The caller should
	// surface this as "no build for this platform" — usually a
	// misconfigured channel, not a transient network issue.
	ErrNoArtifactForHost = errors.New("selfupdate: manifest has no artifact for this host")
	// ErrAlreadyInstalled is returned when the version the manifest
	// advertises is the same as the currently-running version. The
	// caller treats this as a no-op, not an error.
	ErrAlreadyInstalled = errors.New("selfupdate: manifest version is already installed")
	// ErrBadSHA256 is returned when the downloaded artifact's SHA-256
	// differs from the one recorded in the signed manifest. Surfaced
	// as HTTP 502 by the handler: the manifest is signed, so a hash
	// mismatch means the artifact was tampered with in transit.
	ErrBadSHA256 = errors.New("selfupdate: artifact sha256 mismatch")
	// ErrSizeMismatch is returned when the downloaded byte count does
	// not match Manifest.Artifact.Size. Prevents a truncated or
	// oversized transfer from slipping through even before the final
	// SHA check.
	ErrSizeMismatch = errors.New("selfupdate: artifact size mismatch")
	// ErrBadTarball is returned for malformed tar archives: unknown
	// entry types, path traversal attempts, or entries outside the
	// expected top-level directory.
	ErrBadTarball = errors.New("selfupdate: tarball is malformed or unsafe")
)
