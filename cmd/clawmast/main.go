// Command clawmast is the ClawMast worker process.
//
// It is the long-running agent clawmastd supervises. In production
// clawmast hosts the local HTTP API, the embedded React+Vite UI, the
// PTY bridge, the gateway orchestrator, and (optionally) the cloud
// tunnel client. During Iteration 0 it ships only the handshake with
// the supervisor — enough to exercise Starting → Running → Stopping
// end-to-end (architecture/supervisor-protocol.md §4).
//
// Cross-platform by design (AGENTS.md R5): the worker compiles on
// Linux, macOS and Windows. On Windows there is no supervisor so the
// run loop simply runs unsupervised.
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/clawmast/clawmast/internal/version"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "-v", "--version":
			fmt.Println(version.Full())
			return
		case "help", "-h", "--help":
			printUsage(os.Stdout)
			return
		case "status":
			os.Exit(runStatus(os.Args[2:], os.Stdout, os.Stderr))
		case "doctor":
			os.Exit(runDoctor(os.Args[2:], os.Stdout, os.Stderr))
		case "logs":
			os.Exit(runLogs(os.Args[2:], os.Stdout, os.Stderr))
		case "open":
			os.Exit(runOpen(os.Args[2:], os.Stdout, os.Stderr))
		}
	}

	level := parseLogLevel(os.Getenv("CLAWMAST_LOG_LEVEL"))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	ctx, stop := workerSignalContext()
	defer stop()

	if err := runWorker(ctx, os.Stdout, logger); err != nil {
		var xe *exitError
		if errors.As(err, &xe) {
			// A typed exit — do not print the generic "clawmast: ..."
			// prefix because the supervisor uses the raw code, not
			// stderr, to drive its state machine.
			logger.Info("worker exiting with reserved code",
				"component", "worker",
				"code", xe.code,
				"reason", xe.reason)
			os.Exit(xe.code)
		}
		fmt.Fprintln(os.Stderr, "clawmast:", err)
		os.Exit(1)
	}
}

// parseLogLevel mirrors clawmastd's handling: unknown values fall
// back to info so a typo does not crash the worker.
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, "Usage: clawmast [subcommand]")
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  status    Show worker + supervisor state and the UI URL")
	fmt.Fprintln(w, "  doctor    Run diagnostics (openclaw discovery, ports, token)")
	fmt.Fprintln(w, "  logs      Print supervisor + worker log tails (use -f to follow)")
	fmt.Fprintln(w, "  open      Open the local UI in the default browser")
	fmt.Fprintln(w, "  version   Print build metadata and exit")
	fmt.Fprintln(w, "  help      Print this help and exit")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Without a subcommand, clawmast runs the worker loop. When")
	fmt.Fprintln(w, "launched under clawmastd (NOTIFY_SOCKET in environment), it")
	fmt.Fprintln(w, "sends READY=1, WATCHDOG=1 and STOPPING=1 per")
	fmt.Fprintln(w, "architecture/supervisor-protocol.md §5.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Environment:")
	fmt.Fprintln(w, "  CLAWMAST_LOG_LEVEL   debug | info | warn | error (default info)")
	fmt.Fprintln(w, "  CLAWMAST_HOME        Install root for subcommands (default:")
	fmt.Fprintln(w, "                       $CLAWMAST_HOME, ~/.clawmast if present,")
	fmt.Fprintln(w, "                       otherwise ~/.local/share/clawmast).")
}
