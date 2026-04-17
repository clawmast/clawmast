//go:build unix

package supervisor

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHistoryAppendRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "history.json")
	h, err := OpenHistory(path)
	if err != nil {
		t.Fatalf("OpenHistory: %v", err)
	}

	entries := []HistoryEntry{
		{Event: EventSpawn, Version: "v0.1.0"},
		{Event: EventCrash, Version: "v0.1.0", ExitCode: IntPtr(1), Reason: "panic"},
		{Event: EventRollback, Version: "v0.1.0", Reason: "crash-loop"},
		{Event: EventStop, Version: "v0.1.0", ExitCode: IntPtr(0)},
	}
	for _, e := range entries {
		if err := h.Append(e); err != nil {
			t.Fatalf("Append %+v: %v", e, err)
		}
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open written file: %v", err)
	}
	defer f.Close()

	var got []HistoryEntry
	s := bufio.NewScanner(f)
	for s.Scan() {
		var e HistoryEntry
		if err := json.Unmarshal(s.Bytes(), &e); err != nil {
			t.Fatalf("unmarshal line %q: %v", s.Text(), err)
		}
		got = append(got, e)
	}
	if len(got) != len(entries) {
		t.Fatalf("line count: got %d want %d", len(got), len(entries))
	}
	for i, e := range got {
		if e.Event != entries[i].Event {
			t.Errorf("entry %d Event=%q want %q", i, e.Event, entries[i].Event)
		}
		if e.Timestamp.IsZero() {
			t.Errorf("entry %d timestamp was not populated", i)
		}
	}
}

func TestHistoryPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s", "history.json")
	h, err := OpenHistory(path)
	if err != nil {
		t.Fatalf("OpenHistory: %v", err)
	}
	if err := h.Append(HistoryEntry{Event: EventSpawn, Version: "v0"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("world/group bits set on history file: %v", info.Mode())
	}
}

func TestHistoryConcurrentAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.json")
	h, err := OpenHistory(path)
	if err != nil {
		t.Fatalf("OpenHistory: %v", err)
	}

	const goroutines = 8
	const perGoroutine = 25
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			for j := 0; j < perGoroutine; j++ {
				if err := h.Append(HistoryEntry{
					Timestamp: time.Now().UTC(),
					Event:     EventSpawn,
					Version:   "v0",
				}); err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}(i)
	}
	for i := 0; i < goroutines; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("goroutine: %v", err)
		}
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	count := 0
	for s.Scan() {
		var e HistoryEntry
		if err := json.Unmarshal(s.Bytes(), &e); err != nil {
			t.Fatalf("line %d not valid JSON: %v (%q)", count, err, s.Text())
		}
		count++
	}
	if count != goroutines*perGoroutine {
		t.Fatalf("want %d lines, got %d", goroutines*perGoroutine, count)
	}
}
