package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ChannelFileName is the basename of the channel-preference file
// written under <InstallRoot>/state/. It carries the user's explicit
// choice between update channels and survives worker restarts, so a
// channel switch from the UI sticks even though env vars are only read
// once per process.
const ChannelFileName = "channel"

// ChannelStable and ChannelBeta are the two channels the UI exposes.
// The updater accepts any string the manifest agrees with, but the UI
// persistence layer validates against this closed set so a typo in a
// hand-edited state file cannot silently redirect updates.
const (
	ChannelStable = "stable"
	ChannelBeta   = "beta"
)

// ErrUnknownChannel is returned by ValidateChannel for names outside
// the {stable, beta} set. Kept as a sentinel so the HTTP handler can
// distinguish "bad input" (400) from "filesystem failure" (500).
var ErrUnknownChannel = errors.New("agent: unknown channel")

// ValidateChannel normalises and rejects unknown channel names. It is
// deliberately strict: accepting arbitrary strings on the write path
// would let a typo silently point auto-update at a channel the release
// pipeline does not publish.
func ValidateChannel(name string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	switch n {
	case ChannelStable, ChannelBeta:
		return n, nil
	case "":
		return "", fmt.Errorf("%w: empty", ErrUnknownChannel)
	default:
		return "", fmt.Errorf("%w: %q (expected stable or beta)", ErrUnknownChannel, name)
	}
}

// ReadChannelPreference reads the persisted channel from
// <stateDir>/channel. Returns ("", nil) when the file does not exist
// so callers can cleanly fall back to env / default. Unknown contents
// are treated as "no preference" rather than a hard error: the file is
// written by the UI and losing a channel pick to a corrupt file should
// not prevent the worker from booting.
func ReadChannelPreference(stateDir string) (string, error) {
	if stateDir == "" {
		return "", nil
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, ChannelFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("agent: read channel preference: %w", err)
	}
	n, err := ValidateChannel(string(raw))
	if err != nil {
		return "", nil
	}
	return n, nil
}

// WriteChannelPreference persists the normalised channel name to
// <stateDir>/channel with 0644 permissions. The write is via a
// rename-tempfile dance so an interrupted write cannot leave a
// truncated file behind for the next worker boot.
func WriteChannelPreference(stateDir, name string) (string, error) {
	if stateDir == "" {
		return "", errors.New("agent: state dir unknown")
	}
	n, err := ValidateChannel(name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return "", fmt.Errorf("agent: mkdir state: %w", err)
	}
	final := filepath.Join(stateDir, ChannelFileName)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, []byte(n+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("agent: write channel preference: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("agent: rename channel preference: %w", err)
	}
	return n, nil
}

// ResolveChannel picks the effective channel for this worker boot in
// the order (preference file > env var > fallback). Used by the worker
// entry point to honour a UI pick even when the supervisor spawns with
// CLAWMAST_UPDATE_CHANNEL from the environment.
func ResolveChannel(stateDir, envValue, fallback string) (value string, source string) {
	if stateDir != "" {
		if v, err := ReadChannelPreference(stateDir); err == nil && v != "" {
			return v, "preference-file"
		}
	}
	if envValue != "" {
		if v, err := ValidateChannel(envValue); err == nil {
			return v, "env"
		}
	}
	if fallback == "" {
		fallback = ChannelStable
	}
	return fallback, "default"
}
