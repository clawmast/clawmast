// Package history is the cross-platform reader for the supervisor's
// on-disk history ledger (state/history.json).
//
// The supervisor writes the file as a JSON-lines stream, one entry per
// line, via internal/supervisor.History. That package is //go:build
// unix because the supervisor itself is POSIX-only (AGENTS.md R5).
// The worker — which must keep compiling under GOOS=windows — cannot
// import it, so this package duplicates the on-disk schema and a
// read-only tail function.
//
// Schema is frozen in architecture/supervisor-protocol.md §8. Keep
// Entry's JSON tags in lock step with supervisor.HistoryEntry.
package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Event is the on-disk discriminator. Values match supervisor.HistoryEvent.
type Event string

const (
	EventSpawn    Event = "spawn"
	EventCrash    Event = "crash"
	EventRollback Event = "rollback"
	EventStop     Event = "stop"
)

// Entry is one record in state/history.json. Optional fields are
// elided on write (supervisor side) when zero; on read we decode them
// as zero-values or nil.
type Entry struct {
	Timestamp time.Time `json:"ts"`
	Event     Event     `json:"event"`
	Version   string    `json:"version,omitempty"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// ReadOptions tunes ReadTail. Zero values request sensible defaults:
// unlimited entries, no max bytes, skip unparseable lines.
type ReadOptions struct {
	// Limit caps the number of entries returned (tail-style). Zero
	// means "no limit". Negative values are treated as zero.
	Limit int
	// Strict causes parse errors to stop the read and bubble up.
	// When false (default) malformed lines are skipped — a partially
	// written trailing record from a power loss should not break
	// history retrieval.
	Strict bool
}

// ReadTail returns up to opts.Limit entries from path, newest last.
// A missing file is not an error: the function returns (nil, nil) so
// callers can render an empty history page without a disk probe.
func ReadTail(path string, opts ReadOptions) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("history: open %s: %w", path, err)
	}
	defer f.Close()

	entries, err := decodeAll(f, opts.Strict)
	if err != nil {
		return nil, err
	}
	if opts.Limit > 0 && len(entries) > opts.Limit {
		entries = entries[len(entries)-opts.Limit:]
	}
	return entries, nil
}

// decodeAll reads the entire file, one line per entry. It uses a
// bufio.Scanner with an enlarged buffer so pathologically long lines
// still decode cleanly.
func decodeAll(r io.Reader, strict bool) ([]Entry, error) {
	sc := bufio.NewScanner(r)
	// Allow up to 256 KiB per line — well above any realistic history
	// entry and under the default syscall write granularity.
	sc.Buffer(make([]byte, 0, 64*1024), 256*1024)

	var out []Entry
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			if strict {
				return nil, fmt.Errorf("history: decode: %w", err)
			}
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("history: scan: %w", err)
	}
	return out, nil
}
