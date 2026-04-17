package blacklist

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bl.json")
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load on missing file should not error, got %v", err)
	}
	if got != nil {
		t.Fatalf("want nil entries, got %+v", got)
	}
}

func TestAddAndContains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bl.json")
	entries, err := Add(path, Entry{Version: "v0.0.2", Reason: "user-marked-bad"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if !Contains(entries, "v0.0.2") {
		t.Fatalf("Contains should find v0.0.2")
	}
	if Contains(entries, "v0.0.1") {
		t.Fatalf("Contains should not find v0.0.1")
	}
	if Contains(entries, "") {
		t.Fatalf("Contains must reject empty version")
	}
	if entries[0].Timestamp.IsZero() {
		t.Fatalf("Add should populate Timestamp")
	}
}

func TestAddDedupesReplacingReason(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bl.json")
	if _, err := Add(path, Entry{Version: "v1", Reason: "first"}); err != nil {
		t.Fatal(err)
	}
	entries, err := Add(path, Entry{Version: "v1", Reason: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("Add must dedupe by version, got %d entries", len(entries))
	}
	if entries[0].Reason != "second" {
		t.Fatalf("latest reason must win, got %q", entries[0].Reason)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	// A readable, intact file must exist after Save even if a
	// concurrent reader fires between the writes.
	path := filepath.Join(t.TempDir(), "bl.json")
	in := []Entry{
		{Version: "v1", Timestamp: time.Unix(0, 0).UTC(), Reason: "a"},
		{Version: "v2", Timestamp: time.Unix(1, 0).UTC(), Reason: "b"},
	}
	if err := Save(path, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(out) != 2 || out[0].Version != "v1" || out[1].Version != "v2" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
	// There must be no leftover staging file after a clean Save.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp file should be gone, stat err=%v", err)
	}
}

func TestLoadRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bl.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatalf("expected decode error")
	}
}

func TestAddRequiresVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bl.json")
	if _, err := Add(path, Entry{Reason: "x"}); err == nil {
		t.Fatalf("expected error on empty version")
	}
}
