package openclaw

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeExec writes a tiny executable script into dir and returns the
// path. Tests use this where an honest-to-god executable bit matters
// (Discover's isExecutable check).
func writeExec(t *testing.T, dir, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX exec bits required")
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// TestDiscoverEnvOverride wins over every other strategy when the env
// var points at an executable. Exercises the highest-priority branch.
func TestDiscoverEnvOverride(t *testing.T) {
	dir := t.TempDir()
	exe := writeExec(t, dir, "openclaw-env")
	t.Setenv("CLAWMAST_OPENCLAW_BIN", exe)
	// Scrub PATH so the LookPath branch cannot accidentally succeed.
	t.Setenv("PATH", t.TempDir())

	d := Discover("")
	if d.Source != SourceEnv {
		t.Fatalf("source: got %q want env", d.Source)
	}
	if d.Path != exe {
		t.Fatalf("path: got %q want %q", d.Path, exe)
	}
}

// stubBareCandidates replaces the bareCandidatesFn hook for the
// lifetime of the test so absolute /opt/homebrew probes do not leak
// the host's real openclaw install into the assertion surface.
func stubBareCandidates(t *testing.T, paths []string) {
	t.Helper()
	prev := bareCandidatesFn
	bareCandidatesFn = func() []string { return paths }
	t.Cleanup(func() { bareCandidatesFn = prev })
}

// TestDiscoverEnvIgnoredWhenMissing falls through to the next strategy
// if the env var points at nothing executable. The legacy "env var
// wins unconditionally" behaviour would hide install failures.
func TestDiscoverEnvIgnoredWhenMissing(t *testing.T) {
	stubBareCandidates(t, nil)
	t.Setenv("CLAWMAST_OPENCLAW_BIN", "/definitely/not/here/openclaw")
	t.Setenv("PATH", t.TempDir())
	d := Discover("")
	if d.Source != SourceNone {
		t.Fatalf("source: got %q want none", d.Source)
	}
}

// TestDiscoverOverrideFile reads <stateDir>/openclaw.path when set.
// This is the mode operators use when no env var is wired and PATH
// under launchd still does not include the Node toolchain.
func TestDiscoverOverrideFile(t *testing.T) {
	binDir := t.TempDir()
	exe := writeExec(t, binDir, "openclaw")
	stateDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(stateDir, OverrideFileName),
		[]byte("# comment\n\n"+exe+"\n"),
		0o644,
	); err != nil {
		t.Fatalf("write override: %v", err)
	}
	t.Setenv("CLAWMAST_OPENCLAW_BIN", "")
	t.Setenv("PATH", t.TempDir())

	d := Discover(stateDir)
	if d.Source != SourceOverride {
		t.Fatalf("source: got %q want override", d.Source)
	}
	if d.Path != exe {
		t.Fatalf("path: got %q want %q", d.Path, exe)
	}
}

// TestDiscoverPathHit exercises the LookPath branch end-to-end. A
// resolved path inside a tempdir classifies as SourcePath since it
// does not match any well-known prefix — TestClassifyPath covers the
// labelled-location cases without needing real /opt/homebrew.
func TestDiscoverPathHit(t *testing.T) {
	stubBareCandidates(t, nil)
	dir := t.TempDir()
	exe := writeExec(t, dir, "openclaw")
	t.Setenv("CLAWMAST_OPENCLAW_BIN", "")
	t.Setenv("PATH", dir)

	d := Discover("")
	if d.Path != exe {
		t.Fatalf("path: got %q want %q", d.Path, exe)
	}
	if d.Source != SourcePath {
		t.Fatalf("source: got %q want path", d.Source)
	}
}

// TestDiscoverNone returns SourceNone when nothing is found. The
// worker maps this to CLIMissing=true on the Snapshot.
func TestDiscoverNone(t *testing.T) {
	stubBareCandidates(t, nil)
	t.Setenv("CLAWMAST_OPENCLAW_BIN", "")
	t.Setenv("PATH", t.TempDir())

	d := Discover("")
	if d.Source != SourceNone {
		t.Fatalf("source: got %q want none", d.Source)
	}
	if d.Path != "" {
		t.Fatalf("path: want empty got %q", d.Path)
	}
}

// TestClassifyPath exercises each branch of the classifier without
// touching the filesystem.
func TestClassifyPath(t *testing.T) {
	cases := []struct {
		in   string
		want DiscoverySource
	}{
		{"/opt/homebrew/bin/openclaw", SourceBrew},
		{"/usr/local/bin/openclaw", SourceBrew},
		{"/home/user/.local/bin/openclaw", SourceUserBin},
		{"/home/user/.fnm/node-versions/v20/installation/bin/openclaw", SourceNodeMgr},
		{"/home/user/.nvm/versions/node/v22/bin/openclaw", SourceNodeMgr},
		{"/home/user/.bun/bin/openclaw", SourceNodeMgr},
		{"/random/place/openclaw", SourcePath},
	}
	for _, tc := range cases {
		if got := classifyPath(tc.in); got != tc.want {
			t.Errorf("classifyPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
