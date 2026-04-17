package updater

import (
	"encoding/hex"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// Manifest is the JSON shape served at <channel>/manifest.json. The
// release pipeline is the single writer; every field is required
// except Notes.
//
// The document is held intentionally flat so the JS client can render
// "v0.2.1 released 2026-04-20" without a schema layer, and so that
// future tooling (verify CLI, release dry-run) can parse it with the
// stdlib alone.
type Manifest struct {
	// Channel names the release train this manifest belongs to — one
	// of "stable" or "beta" for now. It is mirrored in the URL path
	// so that a signed "stable" manifest served under /beta/ is
	// rejected by Verify.
	Channel string `json:"channel"`

	// Version is the semver of the released artifacts, leading "v"
	// included ("v0.1.0"). Callers compare it against the running
	// version to decide whether an update is available.
	Version string `json:"version"`

	// PublishedAt is the UTC timestamp the manifest was signed. It
	// doubles as a coarse freshness check once we add "don't
	// downgrade" protection.
	PublishedAt time.Time `json:"published_at"`

	// Notes is an optional human-readable changelog blurb the UI
	// shows when an update is available.
	Notes string `json:"notes,omitempty"`

	// Artifacts lists every binary for this release, one per
	// (OS, Arch) pair. Callers use ForCurrentHost to pick the right
	// one for runtime.GOOS / runtime.GOARCH.
	Artifacts []Artifact `json:"artifacts"`
}

// Artifact describes one downloadable binary. URL points at an
// absolute https:// location (usually under the same origin as the
// manifest, but we do not enforce that — detaching download hosting
// from manifest hosting is explicitly allowed by §6 of refactor.md).
// The SHA256 is a second defence-in-depth check performed on top of
// the minisign signature that covers the manifest as a whole.
type Artifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ForCurrentHost returns the artifact matching the running process's
// GOOS / GOARCH, or false if the manifest does not advertise one.
func (m Manifest) ForCurrentHost() (Artifact, bool) {
	return m.ForHost(runtime.GOOS, runtime.GOARCH)
}

// ForHost is the testable form of ForCurrentHost.
func (m Manifest) ForHost(goos, goarch string) (Artifact, bool) {
	for _, a := range m.Artifacts {
		if a.OS == goos && a.Arch == goarch {
			return a, true
		}
	}
	return Artifact{}, false
}

// Validate checks structural invariants that are cheap to enforce
// client-side. Minisign only guarantees that the bytes came from the
// holder of the signing key; it does not promise that the bytes form
// a sensible manifest. Callers should run Validate after Verify.
func (m Manifest) Validate() error {
	if m.Channel == "" {
		return fmt.Errorf("updater: manifest channel is empty")
	}
	if !strings.HasPrefix(m.Version, "v") {
		return fmt.Errorf("updater: manifest version %q is missing the leading \"v\"", m.Version)
	}
	if m.PublishedAt.IsZero() {
		return fmt.Errorf("updater: manifest published_at is zero")
	}
	if len(m.Artifacts) == 0 {
		return fmt.Errorf("updater: manifest has no artifacts")
	}
	for i, a := range m.Artifacts {
		if a.OS == "" || a.Arch == "" {
			return fmt.Errorf("updater: manifest artifact %d missing os/arch", i)
		}
		if !strings.HasPrefix(a.URL, "https://") && !strings.HasPrefix(a.URL, "http://127.0.0.1") {
			return fmt.Errorf("updater: manifest artifact %d %s/%s URL must be https (got %q)", i, a.OS, a.Arch, a.URL)
		}
		if a.Size <= 0 {
			return fmt.Errorf("updater: manifest artifact %d %s/%s size must be > 0", i, a.OS, a.Arch)
		}
		if _, err := hex.DecodeString(a.SHA256); err != nil || len(a.SHA256) != 64 {
			return fmt.Errorf("updater: manifest artifact %d %s/%s sha256 must be 64 hex chars", i, a.OS, a.Arch)
		}
	}
	return nil
}
