// `clawmast status` — one-screen snapshot of the local install. Always
// exits 0 when the install tree exists (the command's job is to
// describe reality, not to fail); non-zero is reserved for I/O errors
// that keep it from reading the tree at all.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/clawmast/clawmast/internal/openclaw"
	"github.com/clawmast/clawmast/internal/version"
)

// versionPayload mirrors the subset of /api/version the CLI cares
// about. Kept local to this file so the agent package stays free of a
// "CLI consumer" concept.
type versionPayload struct {
	Version           string `json:"version"`
	SupervisorVersion string `json:"supervisor_version"`
	InstallRoot       string `json:"install_root"`
	Channel           string `json:"channel"`
}

// healthPayload mirrors /api/health. Kept separate from versionPayload
// because /api/health is auth-exempt — the CLI can read uptime even
// when the token file got wiped.
type healthPayload struct {
	OK       bool  `json:"ok"`
	UptimeMS int64 `json:"uptime_ms"`
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	_ = args // no flags yet; future --json lives here
	root := resolveCLIInstallRoot()
	if root == "" {
		fmt.Fprintln(stderr, "clawmast: could not determine install root")
		return 1
	}
	stateDir := filepath.Join(root, "state")
	url := resolveAgentURL()
	token, fp := loadBearerToken(stateDir)

	// Header block — identity of this install tree.
	fmt.Fprintf(stdout, "clawmast %s\n", version.Full())
	fmt.Fprintf(stdout, "  install root : %s\n", root)
	fmt.Fprintf(stdout, "  ui url       : %s\n", url)
	if fp != "" {
		fmt.Fprintf(stdout, "  bearer token : %s (stored in %s/token)\n", fp, stateDir)
	} else {
		fmt.Fprintf(stdout, "  bearer token : (none — has clawmastd ever booted here?)\n")
	}

	// Daemon block — probe /api/version for identity and /api/health
	// for uptime. /api/health is auth-exempt so it still reports
	// liveness even when the token file went missing.
	if payload, err := fetchVersionPayload(url, token, 2*time.Second); err == nil {
		fmt.Fprintln(stdout, "  daemon       : running")
		if payload.SupervisorVersion != "" {
			fmt.Fprintf(stdout, "  supervisor   : %s\n", payload.SupervisorVersion)
		}
		if payload.Channel != "" {
			fmt.Fprintf(stdout, "  channel      : %s\n", payload.Channel)
		}
		if h, err := fetchHealthPayload(url, 2*time.Second); err == nil && h.UptimeMS > 0 {
			fmt.Fprintf(stdout, "  uptime       : %s\n", humanDuration(h.UptimeMS/1000))
		}
	} else {
		fmt.Fprintf(stdout, "  daemon       : unreachable (%s)\n", shortErr(err))
		fmt.Fprintf(stdout, "                 hint: run `clawmast doctor` for details\n")
	}

	// OpenClaw block — same discovery logic the poller uses, run inline
	// so the output matches what the daemon sees.
	disc := openclaw.Discover(stateDir)
	if disc.Path != "" {
		fmt.Fprintf(stdout, "  openclaw     : %s (%s)\n", disc.Path, disc.Source)
	} else {
		fmt.Fprintf(stdout, "  openclaw     : not found\n")
		fmt.Fprintf(stdout, "                 set CLAWMAST_OPENCLAW_BIN or write the\n")
		fmt.Fprintf(stdout, "                 absolute path to %s\n",
			filepath.Join(stateDir, openclaw.OverrideFileName))
	}
	return 0
}

// fetchVersionPayload hits /api/version on the local daemon. It does
// not follow redirects — the endpoint is served directly by the worker.
// A non-200 response surfaces as an error so the caller prints the
// "unreachable" branch.
func fetchVersionPayload(baseURL, token string, timeout time.Duration) (*versionPayload, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/version", nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if err != nil {
		return nil, err
	}
	var p versionPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// humanDuration renders seconds as a short "2d3h", "4h12m", "37s"
// string, matching the dashboard's System card row.
func humanDuration(secs int64) string {
	d := time.Duration(secs) * time.Second
	if d >= 24*time.Hour {
		days := d / (24 * time.Hour)
		hours := (d % (24 * time.Hour)) / time.Hour
		return fmt.Sprintf("%dd%dh", days, hours)
	}
	if d >= time.Hour {
		h := d / time.Hour
		m := (d % time.Hour) / time.Minute
		return fmt.Sprintf("%dh%dm", h, m)
	}
	if d >= time.Minute {
		m := d / time.Minute
		s := (d % time.Minute) / time.Second
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}

// shortErr strips the common "dial tcp 127.0.0.1:17080: " prefix from
// connection errors so the status line stays one row wide.
func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i > 0 && i < len(s)-2 {
		return s[i+2:]
	}
	return s
}

// fetchHealthPayload mirrors fetchVersionPayload for the auth-exempt
// /api/health surface. Kept separate so status can still report uptime
// after a token reset wipes the authenticated surface.
func fetchHealthPayload(baseURL string, timeout time.Duration) (*healthPayload, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(strings.TrimRight(baseURL, "/") + "/api/health")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if err != nil {
		return nil, err
	}
	var h healthPayload
	if err := json.Unmarshal(body, &h); err != nil {
		return nil, err
	}
	return &h, nil
}
