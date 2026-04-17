// Command clawmastd is the ClawMast supervisor.
//
// It spawns and supervises the clawmast worker, health-checks it,
// activates new worker versions atomically via symlink swap, and rolls
// back on health failures or repeated crashes. It uses the Go standard
// library only, so that it stays frozen while worker functionality
// evolves behind it.
//
// The supervisor is POSIX-only (macOS and Linux, Tier 1 per AGENTS.md R5
// and architecture/client-topology.md §2). The Windows build of this
// command exists so the binary still compiles cross-platform, but it
// only serves the `version` subcommand and refuses to run the
// supervisor loop.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/clawmast/clawmast/internal/version"
)

// exitError is the platform-neutral error type main recognises to
// translate supervisor-visible failures into OS-visible exit codes
// without pulling the unix-only supervisor package into the windows
// build.
type exitError struct {
	code   int
	reason string
}

func (e *exitError) Error() string {
	if e.reason == "" {
		return fmt.Sprintf("exit code %d", e.code)
	}
	return e.reason
}

// signalContext returns a context that is cancelled on SIGINT or
// SIGTERM (the two signals launchd/systemd use to ask for graceful
// shutdown per architecture/supervisor-protocol.md §7).
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// exitCodeFor maps run()'s return error into the process exit code.
// Code 1 is the generic failure; supervisor-protocol.md §6.3 reserves
// 64 (no-restart) and 65 (crash-loop with no rollback).
func exitCodeFor(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return 1
}

// options is the parsed CLI surface. Populated by parseFlags and then
// handed to the platform-specific run().
type options struct {
	installRoot string
	logLevel    slog.Level
}

func main() {
	// Accept `version` as a leading subcommand on every OS so the
	// smoke test (`make smoke`) keeps working on Windows too.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "-v", "--version":
			fmt.Println(version.Full())
			return
		case "help", "-h", "--help":
			printUsage(os.Stdout)
			return
		}
	}

	opts, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawmastd:", err)
		printUsage(os.Stderr)
		os.Exit(2)
	}

	ctx, stop := signalContext()
	defer stop()

	if err := run(ctx, opts); err != nil {
		// Platform-specific run() returns a sentinel error that
		// carries the OS-visible exit code (protocol §6.3). Keep
		// stdlib-only to stay consistent with the rest of the
		// supervisor.
		code := exitCodeFor(err)
		fmt.Fprintln(os.Stderr, "clawmastd:", err)
		os.Exit(code)
	}
}

// parseFlags wires the flag parser. Defaults follow refactor.md §5:
// install root is ~/.clawmast unless CLAWMAST_HOME or -install-root
// override it.
func parseFlags(argv []string) (options, error) {
	fs := flag.NewFlagSet("clawmastd", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	defaultRoot := os.Getenv("CLAWMAST_HOME")
	if defaultRoot == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			defaultRoot = filepath.Join(home, ".clawmast")
		}
	}
	defaultLevel := os.Getenv("CLAWMAST_LOG_LEVEL")
	if defaultLevel == "" {
		defaultLevel = "info"
	}

	root := fs.String("install-root", defaultRoot, "ClawMast install root (default $CLAWMAST_HOME or ~/.clawmast)")
	levelStr := fs.String("log-level", defaultLevel, "log level: debug, info, warn, error")

	if err := fs.Parse(argv); err != nil {
		return options{}, err
	}
	if *root == "" {
		return options{}, fmt.Errorf("install-root is required (set -install-root or $CLAWMAST_HOME)")
	}
	lvl, err := parseLogLevel(*levelStr)
	if err != nil {
		return options{}, err
	}
	return options{installRoot: *root, logLevel: lvl}, nil
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q", s)
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, "Usage: clawmastd [flags]")
	fmt.Fprintln(w, "       clawmastd version")
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  -install-root PATH   ClawMast install root (default $CLAWMAST_HOME or ~/.clawmast)")
	fmt.Fprintln(w, "  -log-level LEVEL     debug | info | warn | error (default info)")
}
