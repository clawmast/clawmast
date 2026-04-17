package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var data []byte
	for _, l := range lines {
		data = append(data, []byte(l+"\n")...)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestReadTailMissingFileReturnsEmpty(t *testing.T) {
	entries, err := ReadTail(filepath.Join(t.TempDir(), "missing.json"), ReadOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("want 0 entries, got %d", len(entries))
	}
}

func TestReadTailDecodesSupervisorShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.json")
	// Shape matches what internal/supervisor/history.go writes in
	// production (see architecture/supervisor-protocol.md §8).
	writeLines(t, path,
		`{"ts":"2026-04-17T12:00:00Z","event":"spawn","version":"v0.0.1"}`,
		`{"ts":"2026-04-17T12:00:01Z","event":"crash","version":"v0.0.1","exit_code":1,"reason":"unexpected"}`,
		`{"ts":"2026-04-17T12:00:02Z","event":"rollback","version":"v0.0.1","reason":"crash-loop"}`,
		`{"ts":"2026-04-17T12:00:03Z","event":"spawn","version":"v0.0.2"}`,
	)

	entries, err := ReadTail(path, ReadOptions{})
	if err != nil {
		t.Fatalf("ReadTail: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("want 4 entries, got %d", len(entries))
	}
	if entries[0].Event != EventSpawn || entries[0].Version != "v0.0.1" {
		t.Fatalf("entry[0] = %+v", entries[0])
	}
	if entries[1].Event != EventCrash || entries[1].ExitCode == nil || *entries[1].ExitCode != 1 {
		t.Fatalf("entry[1] = %+v", entries[1])
	}
	if entries[2].Event != EventRollback || entries[2].Reason != "crash-loop" {
		t.Fatalf("entry[2] = %+v", entries[2])
	}
	// Timestamps should be parseable RFC3339.
	if entries[3].Timestamp.Before(time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("entry[3].Timestamp = %v", entries[3].Timestamp)
	}
}

func TestReadTailLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.json")
	writeLines(t, path,
		`{"ts":"2026-04-17T12:00:00Z","event":"spawn","version":"v0.0.1"}`,
		`{"ts":"2026-04-17T12:00:01Z","event":"stop","version":"v0.0.1"}`,
		`{"ts":"2026-04-17T12:00:02Z","event":"spawn","version":"v0.0.2"}`,
	)
	entries, err := ReadTail(path, ReadOptions{Limit: 2})
	if err != nil {
		t.Fatalf("ReadTail: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	// Limit must tail: the two returned entries are the newest ones.
	if entries[0].Event != EventStop || entries[1].Version != "v0.0.2" {
		t.Fatalf("tail order wrong: %+v", entries)
	}
}

func TestReadTailSkipsMalformedNonStrict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.json")
	writeLines(t, path,
		`{"ts":"2026-04-17T12:00:00Z","event":"spawn","version":"v0.0.1"}`,
		`{ this is not json `,
		`{"ts":"2026-04-17T12:00:02Z","event":"stop","version":"v0.0.1"}`,
	)
	entries, err := ReadTail(path, ReadOptions{})
	if err != nil {
		t.Fatalf("non-strict: want nil err, got %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 parsed entries (junk skipped), got %d", len(entries))
	}

	_, err = ReadTail(path, ReadOptions{Strict: true})
	if err == nil {
		t.Fatal("strict: want error on malformed line, got nil")
	}
}
