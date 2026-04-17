//go:build unix

package supervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// HistoryEvent is the event discriminator used in state/history.json
// per architecture/supervisor-protocol.md §8.
type HistoryEvent string

const (
	EventSpawn    HistoryEvent = "spawn"
	EventCrash    HistoryEvent = "crash"
	EventRollback HistoryEvent = "rollback"
	EventStop     HistoryEvent = "stop"
)

// HistoryEntry is the on-disk schema. Optional fields are emitted
// only when non-zero so consumers can tell spawn events (no exit
// code) apart from crash events at a glance.
type HistoryEntry struct {
	Timestamp time.Time    `json:"ts"`
	Event     HistoryEvent `json:"event"`
	Version   string       `json:"version,omitempty"`
	ExitCode  *int         `json:"exit_code,omitempty"`
	Reason    string       `json:"reason,omitempty"`
}

// History is a concurrency-safe append-only JSON-lines writer. One
// line per entry keeps recovery trivial: a partial tail from a crash
// is discarded on read, never corrupts earlier entries.
type History struct {
	mu   sync.Mutex
	path string
}

// OpenHistory prepares the parent directory and returns a History
// handle. It does not truncate an existing file.
func OpenHistory(path string) (*History, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("supervisor: history mkdir: %w", err)
	}
	return &History{path: path}, nil
}

// Append writes a single entry, calling fsync so the record survives
// a host power loss. Callers must not mutate entry after Append
// returns.
func (h *History) Append(entry HistoryEntry) error {
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("supervisor: history marshal: %w", err)
	}
	line = append(line, '\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("supervisor: history open: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("supervisor: history write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("supervisor: history sync: %w", err)
	}
	return nil
}

// Path returns the backing file path.
func (h *History) Path() string { return h.path }

// IntPtr is a small helper for callers that want to set ExitCode
// from an int without taking its address inline.
func IntPtr(v int) *int { return &v }
