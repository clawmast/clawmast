// Package blacklist is the read+write store for versions the
// supervisor must refuse to spawn.
//
// Layout on disk (<InstallRoot>/state/blacklist.json) is a single
// JSON array so the file is diffable, easy for operators to edit by
// hand, and atomic to rewrite via tmp+rename:
//
//	[
//	  {"version":"v0.0.2","ts":"2026-04-17T10:00:00Z","reason":"user-marked-bad"}
//	]
//
// The package is cross-platform on purpose: the worker
// (cmd/clawmast, Windows-compilable per AGENTS.md R5) writes the
// file when the operator flags a version, and the supervisor
// (clawmastd, POSIX-only) reads it before every spawn. No file
// locking: the worker is the only writer, the supervisor only
// reads, and the atomic rename on write means the supervisor never
// sees a half-written document.
package blacklist

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Entry is one record in blacklist.json. Reason is free-form but
// conventionally one of: "user-marked-bad", "crash-loop",
// "supervisor-auto". Timestamp is when the entry was added.
type Entry struct {
	Version   string    `json:"version"`
	Timestamp time.Time `json:"ts"`
	Reason    string    `json:"reason,omitempty"`
}

// Load reads path and returns the parsed entries, newest last in
// file order. A missing file is not an error: callers get (nil, nil)
// so a fresh install behaves identically to one with an empty list.
func Load(path string) ([]Entry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("blacklist: read %s: %w", path, err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var out []Entry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("blacklist: decode %s: %w", path, err)
	}
	return out, nil
}

// Save rewrites path with entries, preserving order. The write is
// atomic: the new content lands in a sibling tmp file which is then
// renamed over the target, so a reader never observes a partial
// document and a crashed writer leaves the prior file untouched.
func Save(path string, entries []Entry) error {
	if entries == nil {
		entries = []Entry{}
	}
	buf, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("blacklist: encode: %w", err)
	}
	buf = append(buf, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("blacklist: mkdir: %w", err)
	}
	// Use O_EXCL on the tmp path so two concurrent writers cannot
	// race on the same staging name; the caller retries if needed.
	tmp := path + ".tmp"
	_ = os.Remove(tmp) // clear stale tmp from a prior crash
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("blacklist: create tmp: %w", err)
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("blacklist: write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("blacklist: fsync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("blacklist: close tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("blacklist: rename: %w", err)
	}
	return nil
}

// Add loads path, appends e (deduplicated by version: an existing
// entry for e.Version is replaced in place), and atomically rewrites
// the file. Returns the updated slice and any error.
//
// e.Timestamp is zero-filled with time.Now().UTC() when the caller
// did not set it, so the common case is just blacklist.Add(path,
// blacklist.Entry{Version: v, Reason: r}).
func Add(path string, e Entry) ([]Entry, error) {
	if e.Version == "" {
		return nil, errors.New("blacklist: Entry.Version is required")
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	existing, err := Load(path)
	if err != nil {
		return nil, err
	}
	replaced := false
	for i := range existing {
		if existing[i].Version == e.Version {
			existing[i] = e
			replaced = true
			break
		}
	}
	if !replaced {
		existing = append(existing, e)
	}
	if err := Save(path, existing); err != nil {
		return nil, err
	}
	return existing, nil
}

// Contains reports whether entries has any record for version. An
// empty version string is never contained (defensive against the
// "every version is blacklisted" footgun when the caller fails to
// resolve a version label).
func Contains(entries []Entry, version string) bool {
	if version == "" {
		return false
	}
	for i := range entries {
		if entries[i].Version == version {
			return true
		}
	}
	return false
}
