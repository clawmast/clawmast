package openclaw

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// DiscoverySource labels how the openclaw binary was located. The UI
// surfaces this string so the operator can reason about which copy of
// the CLI the worker is driving — especially useful when multiple Node
// toolchains on the host each ship their own `npm install -g` copy.
type DiscoverySource string

const (
	// SourceNone is the sentinel "no openclaw anywhere we looked" result.
	SourceNone DiscoverySource = "none"
	// SourceEnv: CLAWMAST_OPENCLAW_BIN pointed us at an executable.
	SourceEnv DiscoverySource = "env"
	// SourceOverride: <state>/openclaw.path contained an absolute path
	// to an executable. Same priority idea as the env var but persists
	// across supervisor restarts without touching the plist.
	SourceOverride DiscoverySource = "override"
	// SourcePath: exec.LookPath("openclaw") succeeded. Honoured whatever
	// PATH the worker inherited (usually narrower than a login shell
	// under launchd / systemd, which is why bare-dir probes come next).
	SourcePath DiscoverySource = "path"
	// SourceBrew: resolved under /opt/homebrew/bin or /usr/local/bin.
	SourceBrew DiscoverySource = "brew"
	// SourceUserBin: resolved under ~/.local/bin or other XDG-style
	// per-user bindirs.
	SourceUserBin DiscoverySource = "user-bin"
	// SourceNodeMgr: resolved under a Node version manager tree
	// (fnm, nvm, volta, asdf, ~/.bun).
	SourceNodeMgr DiscoverySource = "node-mgr"
)

// Discovery is one lookup outcome. A zero Path paired with SourceNone
// means "nothing found" — callers treat that identically to the legacy
// "binary not on PATH" case.
type Discovery struct {
	Path   string
	Source DiscoverySource
}

// OverrideFileName is the basename of the state-directory override file
// that Discover consults after the env var. Keeping it a package-level
// constant so CLI tools (doctor, status) can document the same path
// without drifting.
const OverrideFileName = "openclaw.path"

// Discover locates the openclaw executable using a fixed priority chain:
//
//  1. CLAWMAST_OPENCLAW_BIN env var (explicit operator override)
//  2. <stateDir>/openclaw.path override file (survives restarts)
//  3. exec.LookPath("openclaw") — narrow-PATH launchd/systemd env
//  4. Well-known fixed locations: Homebrew, XDG ~/.local/bin
//  5. Per-user Node toolchains: fnm, nvm, volta, asdf, ~/.bun
//
// It never spawns a subprocess. Filesystem probes are cheap (~10 stats)
// so the poller can call Discover on every tick without caching.
// stateDir may be empty — the override-file strategy is simply skipped.
func Discover(stateDir string) Discovery {
	if p := strings.TrimSpace(os.Getenv("CLAWMAST_OPENCLAW_BIN")); p != "" && isExecutable(p) {
		return Discovery{Path: p, Source: SourceEnv}
	}
	if stateDir != "" {
		if p := readOverride(filepath.Join(stateDir, OverrideFileName)); p != "" && isExecutable(p) {
			return Discovery{Path: p, Source: SourceOverride}
		}
	}
	if p, err := exec.LookPath(DefaultBinary); err == nil {
		return Discovery{Path: p, Source: classifyPath(p)}
	}
	for _, cand := range bareCandidatesFn() {
		if isExecutable(cand) {
			return Discovery{Path: cand, Source: classifyPath(cand)}
		}
	}
	return Discovery{Source: SourceNone}
}

// bareCandidatesFn is the seam tests use to stub the hard-coded
// absolute paths (which cannot be sandboxed into a tempdir). Production
// callers read it as-is.
var bareCandidatesFn = bareCandidates

// bareCandidates enumerates well-known absolute locations in priority
// order. Paths under $HOME are only added when os.UserHomeDir succeeds;
// a failure there just drops the per-user candidates rather than
// aborting the whole probe.
func bareCandidates() []string {
	var out []string
	if runtime.GOOS != "windows" {
		out = append(out,
			"/opt/homebrew/bin/openclaw",
			"/usr/local/bin/openclaw",
		)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return out
	}
	out = append(out,
		filepath.Join(home, ".local", "bin", "openclaw"),
		filepath.Join(home, ".bun", "bin", "openclaw"),
		filepath.Join(home, ".volta", "bin", "openclaw"),
		filepath.Join(home, ".npm-global", "bin", "openclaw"),
	)
	// Node version managers install npm globals under a version-keyed
	// subdirectory. One level of directory listing per root is cheap
	// and catches the common shape without needing to spawn `node -p`.
	type vroot struct {
		root   string
		suffix []string
	}
	roots := []vroot{
		{filepath.Join(home, ".fnm", "node-versions"), []string{"installation/bin/openclaw"}},
		{filepath.Join(home, ".nvm", "versions", "node"), []string{"bin/openclaw"}},
		{filepath.Join(home, ".asdf", "installs", "nodejs"), []string{"bin/openclaw"}},
	}
	for _, r := range roots {
		entries, err := os.ReadDir(r.root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			for _, suf := range r.suffix {
				out = append(out, filepath.Join(r.root, e.Name(), suf))
			}
		}
	}
	return out
}

// classifyPath labels an absolute path with the best-guess source. Only
// used when the underlying resolution did not produce a more specific
// label (env/override). Falls back to SourcePath when nothing matches,
// which means "on PATH but in a location we do not recognise".
func classifyPath(p string) DiscoverySource {
	switch {
	case strings.HasPrefix(p, "/opt/homebrew/"), strings.HasPrefix(p, "/usr/local/"):
		return SourceBrew
	case strings.Contains(p, "/.local/bin/"):
		return SourceUserBin
	case strings.Contains(p, "/.fnm/"),
		strings.Contains(p, "/.nvm/"),
		strings.Contains(p, "/.volta/"),
		strings.Contains(p, "/.asdf/"),
		strings.Contains(p, "/.bun/"),
		strings.Contains(p, "/.npm-global/"):
		return SourceNodeMgr
	}
	return SourcePath
}

// readOverride reads the first non-blank, non-comment line of the
// override file and returns it trimmed. A missing file, I/O error, or
// empty file returns "" — the caller falls back to the next strategy.
func readOverride(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	return ""
}

// isExecutable tests whether path points at a regular file that at
// least one of the owner/group/other execute bits is set on. On
// Windows we only verify existence (no mode bits) — openclaw there is
// a `.cmd` shim that the OS resolves via PATHEXT, not via mode.
func isExecutable(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}
