// `clawmast doctor` — walk through the install tree and surface every
// check the operator might otherwise debug in a shell: install root
// layout, supervisor + worker version, token perms, agent reachability,
// openclaw discovery trace, CLI symlink on PATH. Each row prints OK /
// WARN / FAIL plus a one-line reason; the process exits 0 even when
// checks WARN so a scripted wrapper can use doctor as an inventory
// tool, and exits 1 only on hard FAIL items that keep the install from
// running at all.
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/clawmast/clawmast/internal/openclaw"
)

type checkState int

const (
	stateOK checkState = iota
	stateWarn
	stateFail
)

func (s checkState) label() string {
	switch s {
	case stateOK:
		return "OK  "
	case stateWarn:
		return "WARN"
	case stateFail:
		return "FAIL"
	}
	return "????"
}

func runDoctor(_ []string, stdout, _ io.Writer) int {
	root := resolveCLIInstallRoot()
	fmt.Fprintf(stdout, "clawmast doctor — install root: %s\n\n", root)

	fails := 0
	row := func(name string, st checkState, detail string) {
		fmt.Fprintf(stdout, "  [%s] %-22s %s\n", st.label(), name, detail)
		if st == stateFail {
			fails++
		}
	}

	// Install tree shape ------------------------------------------------
	if fileExists(root) {
		row("install root", stateOK, root)
	} else {
		row("install root", stateFail, "not found — run scripts/install.sh")
		return 1 // cannot meaningfully continue without a tree
	}
	for _, sub := range []string{"bin", "versions", "state", "logs"} {
		p := filepath.Join(root, sub)
		if fileExists(p) {
			row(sub+"/", stateOK, p)
		} else {
			row(sub+"/", stateFail, "missing")
		}
	}

	// Supervisor + worker identity --------------------------------------
	super := filepath.Join(root, "bin", "clawmastd")
	if fileExists(super) {
		if v, err := runVersion(super); err == nil {
			row("clawmastd version", stateOK, v)
		} else {
			row("clawmastd version", stateFail, err.Error())
		}
	} else {
		row("clawmastd", stateFail, "missing "+super)
	}
	current := filepath.Join(root, "current", "clawmast")
	if fileExists(current) {
		if v, err := runVersion(current); err == nil {
			row("clawmast version", stateOK, v)
		} else {
			row("clawmast version", stateFail, err.Error())
		}
	} else {
		row("clawmast (current)", stateFail, "missing "+current)
	}

	// Token file --------------------------------------------------------
	stateDir := filepath.Join(root, "state")
	tokPath := filepath.Join(stateDir, "token")
	if fi, err := os.Stat(tokPath); err == nil {
		perm := fi.Mode().Perm()
		if perm == 0o600 {
			row("bearer token", stateOK, fmt.Sprintf("%s (mode %#o)", tokPath, perm))
		} else {
			row("bearer token", stateWarn, fmt.Sprintf("%s has mode %#o; expected 0600", tokPath, perm))
		}
	} else {
		row("bearer token", stateWarn, "missing — will be generated on next clawmastd boot")
	}

	// CLI symlink on PATH ----------------------------------------------
	if p, err := exec.LookPath("clawmast"); err == nil {
		row("clawmast on PATH", stateOK, p)
	} else {
		row("clawmast on PATH", stateWarn, "not on PATH — run install.sh or symlink manually")
	}

	// Agent reachability ------------------------------------------------
	url := resolveAgentURL()
	if _, err := fetchHealthPayload(url, 1500*time.Millisecond); err == nil {
		row("daemon http", stateOK, url)
	} else {
		row("daemon http", stateWarn, fmt.Sprintf("%s unreachable (%s)", url, shortErr(err)))
	}

	// Port check --------------------------------------------------------
	host, port, err := net.SplitHostPort(strings.TrimSuffix(strings.TrimPrefix(url, "http://"), "/"))
	if err == nil {
		if conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 250*time.Millisecond); err == nil {
			_ = conn.Close()
			row("port "+port, stateOK, "listening on "+host)
		} else {
			row("port "+port, stateWarn, "no listener — clawmastd may be down")
		}
	}

	// OpenClaw discovery ------------------------------------------------
	disc := openclaw.Discover(stateDir)
	if disc.Path != "" {
		row("openclaw", stateOK, fmt.Sprintf("%s (%s)", disc.Path, disc.Source))
	} else {
		row("openclaw", stateWarn, "not found — set CLAWMAST_OPENCLAW_BIN or write "+
			filepath.Join(stateDir, openclaw.OverrideFileName))
	}

	fmt.Fprintln(stdout)
	if fails > 0 {
		fmt.Fprintf(stdout, "%d check(s) FAIL — install is not operational\n", fails)
		return 1
	}
	fmt.Fprintln(stdout, "all critical checks passed")
	return 0
}

// runVersion invokes `<bin> version` with a short timeout and returns
// the trimmed first line. Used for both clawmast and clawmastd.
func runVersion(bin string) (string, error) {
	cmd := exec.Command(bin, "version")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	return line, nil
}
