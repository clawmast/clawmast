package selfupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// GateMarker is the on-disk trace left behind by a successful self-
// install, read by the supervisor on the next spawn to decide
// whether the HealthGate should be armed for that version.
//
// The file lives at <InstallRoot>/state/install-gate.json. It is
// intentionally tiny — just enough for the supervisor to answer
// "is this spawn still under observation?" without depending on
// selfupdate for duration policy. The supervisor owns window /
// stableFor via its own Config; the marker only records identity
// and the install timestamp.
//
// Cross-platform on purpose: the marker is written by code that may
// run on any platform selfupdate supports, and is read by the unix
// supervisor. Keeping the struct itself build-tag-free avoids
// accidental skew between writer and reader shapes.
type GateMarker struct {
	Version     string    `json:"version"`
	InstalledAt time.Time `json:"installed_at"`
}

// GateMarkerPath returns the canonical on-disk location of the
// install-gate marker for a given install root. Callers that want
// to read / delete the marker without going through the typed
// helpers here can join the same path themselves; exposing this as
// a helper keeps the supervisor and selfupdate halves consistent
// if we ever relocate the file.
func GateMarkerPath(installRoot string) string {
	return filepath.Join(installRoot, "state", "install-gate.json")
}

// WriteGateMarker serialises m to installRoot/state/install-gate.json
// using the same tmp+rename pattern blacklist.Save uses, so a reader
// (the supervisor) never observes a half-written file. The caller
// is responsible for ensuring the state directory exists; under the
// normal path selfupdate.Apply has already created the install tree.
func WriteGateMarker(installRoot string, m GateMarker) error {
	if m.Version == "" {
		return errors.New("selfupdate: gate marker requires version")
	}
	if m.InstalledAt.IsZero() {
		m.InstalledAt = time.Now().UTC()
	}
	path := GateMarkerPath(installRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("selfupdate: mkdir state dir: %w", err)
	}
	buf, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return fmt.Errorf("selfupdate: write gate marker: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("selfupdate: rename gate marker: %w", err)
	}
	return nil
}

// ReadGateMarker returns the marker at installRoot. A missing file
// is not an error: callers get (zero, nil) and treat it as "no
// active install under observation". Any other read or parse error
// is surfaced to the caller, which must decide whether to bail or
// degrade gracefully.
func ReadGateMarker(installRoot string) (GateMarker, error) {
	var m GateMarker
	buf, err := os.ReadFile(GateMarkerPath(installRoot))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return m, nil
		}
		return m, fmt.Errorf("selfupdate: read gate marker: %w", err)
	}
	if err := json.Unmarshal(buf, &m); err != nil {
		return m, fmt.Errorf("selfupdate: parse gate marker: %w", err)
	}
	return m, nil
}

// DeleteGateMarker removes the marker file. A missing file is not
// an error. The supervisor calls this on Pass (the new version
// committed cleanly) and on Fail (after the rollback path has
// blacklisted the version); either way the observation window for
// this install is over.
func DeleteGateMarker(installRoot string) error {
	err := os.Remove(GateMarkerPath(installRoot))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("selfupdate: remove gate marker: %w", err)
	}
	return nil
}
