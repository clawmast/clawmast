// `clawmast open` — launch the default browser at the local UI URL.
// Exists so an install without a tray icon is still one command away
// from the dashboard. Prints the URL first so a headless / SSH shell
// without a browser still gets something useful.
package main

import (
	"fmt"
	"io"
	"os/exec"
	"runtime"
)

func runOpen(_ []string, stdout, stderr io.Writer) int {
	url := resolveAgentURL()
	fmt.Fprintln(stdout, url)

	cmd := browserOpenCmd(url)
	if cmd == nil {
		fmt.Fprintln(stderr, "clawmast: no known browser-launcher on this platform; open the URL above manually")
		return 0
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "clawmast: failed to launch browser: %v\n", err)
		return 1
	}
	// Do not Wait — the browser may fork and detach; we just want the
	// command launched. Release detaches it from us so the CLI returns
	// immediately instead of tying its lifetime to the browser window.
	_ = cmd.Process.Release()
	return 0
}

// browserOpenCmd returns nil on platforms where we have no obvious
// launcher. The caller prints a fallback message; we never guess.
func browserOpenCmd(url string) *exec.Cmd {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url)
	case "linux":
		// xdg-open is the freedesktop.org standard; every major distro
		// installs it by default. We do not fall back to firefox /
		// google-chrome because those names are not universal and a
		// missing xdg-open is the operator's cue that their desktop
		// environment is unusual.
		return exec.Command("xdg-open", url)
	case "windows":
		// rundll32 url.dll,FileProtocolHandler is the POSIX-style way
		// to open a URL on Windows without spawning cmd.exe. Kept
		// here even though the worker is Tier 2 on Windows because
		// the CLI subcommands are the main thing Windows users have.
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	}
	return nil
}
