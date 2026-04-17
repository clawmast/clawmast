//go:build unix

package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newSupervisorFixture builds a Config wired for fast tests: short
// timeouts, the current test binary as the fake worker, and a short
// temp dir for the notify socket.
func newSupervisorFixture(t *testing.T, mode string, opts ...func(*Config)) (*Supervisor, *Config) {
	t.Helper()
	root := shortSocketDir(t)
	histDir := t.TempDir()
	path, extraEnv := fakeWorkerCommand(t, mode)
	cfg := Config{
		InstallRoot:      "", // using explicit overrides to keep socket path short
		WorkerBinaryName: "clawmast",
		ExtraEnv:         extraEnv,
		StartTimeout:     500 * time.Millisecond,
		WatchdogInterval: 200 * time.Millisecond,
		StopGrace:        300 * time.Millisecond,
		Backoff: BackoffPolicy{
			Base:        20 * time.Millisecond,
			Max:         40 * time.Millisecond,
			SteadyReset: time.Minute,
			CrashWindow: time.Minute,
			CrashCap:    3,
		},
		NotifySocketPath: filepath.Join(root, "n.sock"),
		HistoryPath:      filepath.Join(histDir, "history.json"),
		RunDir:           root,
		ResolveWorker: func() (string, string, error) {
			return path, "v0.test", nil
		},
		Stdout: io.Discard,
		Stderr: io.Discard,
	}
	for _, o := range opts {
		o(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, &cfg
}

// readHistory loads the test run's history.json and returns events.
// A missing file is reported as an empty slice so tests can poll for
// expected events without racing the supervisor's own file creation.
func readHistory(t *testing.T, path string) []HistoryEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("open history: %v", err)
	}
	defer f.Close()
	var out []HistoryEntry
	s := bufio.NewScanner(f)
	for s.Scan() {
		var e HistoryEntry
		if err := json.Unmarshal(s.Bytes(), &e); err != nil {
			t.Fatalf("unmarshal %q: %v", s.Text(), err)
		}
		out = append(out, e)
	}
	return out
}

func TestRunStopsGracefullyOnContextCancel(t *testing.T) {
	s, cfg := newSupervisorFixture(t, "ready_heartbeat")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Give the worker enough time to signal READY and send at least
	// one WATCHDOG before shutdown.
	time.Sleep(400 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned non-nil on ctx cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	entries := readHistory(t, cfg.HistoryPath)
	if len(entries) == 0 || entries[0].Event != EventSpawn {
		t.Fatalf("expected first entry to be spawn, got %+v", entries)
	}
	last := entries[len(entries)-1]
	if last.Event != EventStop {
		t.Fatalf("expected last entry to be stop, got %+v", last)
	}
}

func TestRunHonoursExitCode64NoRestart(t *testing.T) {
	s, _ := newSupervisorFixture(t, "no_restart")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.Run(ctx)
	var se *StopError
	if !errors.As(err, &se) || se.Code != 64 {
		t.Fatalf("want StopError{Code:64}, got %v", err)
	}
}

func TestRunHonoursExitCode65Rollback_NoPrevious(t *testing.T) {
	s, _ := newSupervisorFixture(t, "rollback")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.Run(ctx)
	var se *StopError
	if !errors.As(err, &se) || se.Code != 65 {
		t.Fatalf("want StopError{Code:65}, got %v", err)
	}
}

func TestRunStartTimeoutTreatedAsCrash(t *testing.T) {
	// `no_ready` never signals READY. StartTimeout fires, the
	// supervisor kills the worker, records a crash, and would
	// retry — the context cancels us first so we only observe the
	// one crash in the ledger.
	s, cfg := newSupervisorFixture(t, "no_ready", func(c *Config) {
		c.StartTimeout = 150 * time.Millisecond
	})
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_ = s.Run(ctx)

	entries := readHistory(t, cfg.HistoryPath)
	var crashes int
	for _, e := range entries {
		if e.Event == EventCrash {
			crashes++
		}
	}
	if crashes == 0 {
		t.Fatalf("expected at least one crash entry, got %+v", entries)
	}
}

func TestRunSigkillsAfterGrace(t *testing.T) {
	// `ready_ignore_term` signals READY and keeps heart-beating
	// (so the watchdog stays satisfied), but refuses to exit on
	// SIGTERM. The supervisor must escalate to SIGKILL after
	// StopGrace. We assert on the history ledger because the
	// SIGKILL signal number shows up in the Stop entry's ExitCode.
	s, cfg := newSupervisorFixture(t, "ready_ignore_term", func(c *Config) {
		c.WatchdogInterval = 500 * time.Millisecond
		c.StopGrace = 100 * time.Millisecond
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after SIGKILL fallback")
	}

	entries := readHistory(t, cfg.HistoryPath)
	if len(entries) == 0 {
		t.Fatal("empty history")
	}
	last := entries[len(entries)-1]
	if last.Event != EventStop {
		t.Fatalf("last event = %s, want stop", last.Event)
	}
	if last.ExitCode == nil || *last.ExitCode != 9 {
		t.Fatalf("last exit_code = %v, want 9 (SIGKILL)", last.ExitCode)
	}
}

// setupRollbackLayout builds a minimal install-root shape suitable
// for a real SwapCurrentPrevious: two "version" directories each
// containing a symlink to the test binary, with current→bad and
// previous→good so the first spawn cycle runs the failing version.
func setupRollbackLayout(t *testing.T) (root, sockDir, histDir string) {
	t.Helper()
	root = t.TempDir()
	sockDir = shortSocketDir(t)
	histDir = t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	for _, name := range []string{"bad", "good"} {
		dir := filepath.Join(root, "versions", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.Symlink(exe, filepath.Join(dir, "clawmast")); err != nil {
			t.Fatalf("symlink worker: %v", err)
		}
	}
	if err := os.Symlink(filepath.Join("versions", "bad"), filepath.Join(root, "current")); err != nil {
		t.Fatalf("symlink current: %v", err)
	}
	if err := os.Symlink(filepath.Join("versions", "good"), filepath.Join(root, "previous")); err != nil {
		t.Fatalf("symlink previous: %v", err)
	}
	return root, sockDir, histDir
}

func TestRunRollsBackAfterCrashLoop(t *testing.T) {
	root, sockDir, histDir := setupRollbackLayout(t)
	cfg := Config{
		InstallRoot:      root,
		WorkerBinaryName: "clawmast",
		ExtraEnv:         []string{"CMFAKE_MODE=by_path"},
		StartTimeout:     300 * time.Millisecond,
		WatchdogInterval: 300 * time.Millisecond,
		StopGrace:        200 * time.Millisecond,
		Backoff: BackoffPolicy{
			Base:        10 * time.Millisecond,
			Max:         20 * time.Millisecond,
			SteadyReset: time.Minute,
			CrashWindow: time.Minute,
			CrashCap:    3,
		},
		NotifySocketPath: filepath.Join(sockDir, "n.sock"),
		HistoryPath:      filepath.Join(histDir, "history.json"),
		RunDir:           sockDir,
		Stdout:           io.Discard,
		Stderr:           io.Discard,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Wait until the "good" version has been reached and is
	// steadily running, then shut down.
	deadline := time.Now().Add(4 * time.Second)
	var saw bool
	for time.Now().Before(deadline) {
		entries := readHistory(t, cfg.HistoryPath)
		for _, e := range entries {
			if e.Event == EventSpawn && e.Version == "good" {
				saw = true
				break
			}
		}
		if saw {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if !saw {
		cancel()
		<-done
		t.Fatal("never observed spawn of rolled-back version")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned: %v", err)
	}

	entries := readHistory(t, cfg.HistoryPath)
	var spawns, crashes, rollbacks int
	var lastSpawnVersion string
	for _, e := range entries {
		switch e.Event {
		case EventSpawn:
			spawns++
			lastSpawnVersion = e.Version
		case EventCrash:
			crashes++
		case EventRollback:
			rollbacks++
			if e.Version != "bad" {
				t.Errorf("rollback version = %q, want bad", e.Version)
			}
		}
	}
	if rollbacks != 1 {
		t.Errorf("rollback count = %d, want 1", rollbacks)
	}
	if crashes < 3 {
		t.Errorf("crash count = %d, want >= 3", crashes)
	}
	if lastSpawnVersion != "good" {
		t.Errorf("last spawn version = %q, want good", lastSpawnVersion)
	}
	if spawns < 4 {
		t.Errorf("spawn count = %d, want >= 4 (3 bad + >=1 good)", spawns)
	}

	// The symlinks on disk must now point the other way.
	curTarget, _ := os.Readlink(filepath.Join(root, "current"))
	if !strings.HasSuffix(curTarget, "good") {
		t.Errorf("current symlink target = %q, want .../good", curTarget)
	}
}
