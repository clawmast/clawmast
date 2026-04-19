// Shared helpers for the user-facing subcommands (status, doctor,
// logs, open). These run from a TTY, not under the supervisor, so they
// must not mutate state, emit slog lines, or assume the worker HTTP
// server is reachable. Every helper here prefers graceful degradation
// (best-effort lookup + "-") over hard failure.
package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/clawmast/clawmast/internal/agent"
)

// resolveCLIInstallRoot mirrors resolveInstallRoot in run.go but without
// logging (the CLI's stdout belongs to the user, not slog). Resolution
// order matches install.sh so `clawmast status` and the daemon agree on
// which tree to talk about:
//
//  1. CLAWMAST_HOME / CLAWMAST_INSTALL_ROOT explicit override.
//  2. Walk up from os.Executable(): a binary at
//     <root>/versions/vX.Y.Z/clawmast or <root>/bin/clawmast rooted at
//     an install tree (has state/ + versions/ siblings) wins.
//  3. Legacy ~/.clawmast if it has current/.
//  4. XDG ~/.local/share/clawmast.
//
// Returns "" only when nothing plausibly exists — the caller prints an
// explanatory message rather than aborting.
func resolveCLIInstallRoot() string {
	if v := strings.TrimSpace(os.Getenv("CLAWMAST_INSTALL_ROOT")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("CLAWMAST_HOME")); v != "" {
		return v
	}
	if root := rootFromExecutable(); root != "" {
		return root
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	legacy := filepath.Join(home, ".clawmast")
	if _, err := os.Lstat(filepath.Join(legacy, "current")); err == nil {
		return legacy
	}
	xdg := filepath.Join(home, ".local", "share", "clawmast")
	if _, err := os.Stat(xdg); err == nil {
		return xdg
	}
	// Fall back to the XDG path even when absent: doctor/status can
	// then tell the user "the tree doesn't exist yet, run install.sh".
	return xdg
}

// rootFromExecutable walks up from os.Executable() looking for an
// install tree (has versions/ and state/). The CLI symlink at
// /usr/local/bin/clawmast points into <root>/current/clawmast; resolving
// the symlink hands us the real location inside the install tree.
func rootFromExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	// Check up to three parents: <root>/versions/vX/clawmast,
	// <root>/bin/clawmastd, and one extra cushion for a future layout.
	dir := filepath.Dir(exe)
	for i := 0; i < 4; i++ {
		if looksLikeInstallRoot(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func looksLikeInstallRoot(dir string) bool {
	for _, sub := range []string{"state", "versions"} {
		st, err := os.Stat(filepath.Join(dir, sub))
		if err != nil || !st.IsDir() {
			return false
		}
	}
	return true
}

// resolveAgentURL returns the HTTP URL the UI is served at. Respects
// CLAWMAST_HTTP_ADDR the same way run.go does and converts bare
// host:port forms into a full URL with the loopback assumption that
// agent.DefaultAddr already encodes.
func resolveAgentURL() string {
	addr := strings.TrimSpace(os.Getenv("CLAWMAST_HTTP_ADDR"))
	if addr == "" || addr == "off" {
		addr = agent.DefaultAddr
	}
	// Normalise an unset or zero host to loopback so the URL is
	// clickable from the user's terminal.
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Sprintf("http://%s/", addr)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s/", net.JoinHostPort(host, port))
}

// loadBearerToken reads <stateDir>/token and returns a short fingerprint
// plus the raw token. Returns empty strings on any error; callers that
// need the token for HTTP requests treat the empty string as
// "unauthenticated request".
func loadBearerToken(stateDir string) (token, fingerprint string) {
	if stateDir == "" {
		return "", ""
	}
	b, err := os.ReadFile(filepath.Join(stateDir, "token"))
	if err != nil {
		return "", ""
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", ""
	}
	// 8 hex chars is enough for a human to eyeball the token matches
	// without ever pasting the real secret into a log.
	fp := t
	if len(fp) > 8 {
		fp = fp[:8]
	}
	return t, fp + "…"
}

// fileExists returns true when path exists (symlink target need not
// exist — lstat suffices for "is anything there at all").
func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}
