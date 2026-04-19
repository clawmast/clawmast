//go:build unix

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/clawmast/clawmast/internal/supervisor"
	"github.com/clawmast/clawmast/internal/version"
)

// run drives the supervisor state machine against the install root
// in opts. The error return folds the supervisor's StopError (which
// carries protocol-defined exit codes) into the platform-neutral
// exitError main uses to pick an OS exit code.
func run(ctx context.Context, opts options) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: opts.logLevel,
	}))
	slog.SetDefault(logger)

	cfg := supervisor.Config{
		InstallRoot:   opts.installRoot,
		Logger:        logger,
		StartTimeout:  parseDurationEnv("CLAWMAST_START_TIMEOUT", logger),
		GateWindow:    parseDurationEnv("CLAWMAST_GATE_WINDOW", logger),
		GateStableFor: parseDurationEnv("CLAWMAST_GATE_STABLE_FOR", logger),
		// Propagate the supervisor's own build string to the worker
		// so /api/version can expose it as supervisor_version without
		// an extra IPC. The worker inherits os.Environ() via spawn,
		// and ExtraEnv is the documented hook for supervisor-provided
		// context variables.
		ExtraEnv: []string{"CLAWMAST_SUPERVISOR_VERSION=" + version.Full()},
	}
	sup, err := supervisor.New(cfg)
	if err != nil {
		return err
	}

	runErr := sup.Run(ctx)
	var stop *supervisor.StopError
	if errors.As(runErr, &stop) {
		return &exitError{code: stop.Code, reason: stop.Reason}
	}
	return runErr
}

// parseDurationEnv returns the parsed value of an env var or 0 when
// unset / invalid. Zero reaches supervisor.Config.withDefaults as the
// "use protocol default" sentinel, so callers can tune individual
// knobs without having to restate the defaults. Malformed values are
// logged at warn but never fatal: this is a smoke-test escape hatch,
// not a load-bearing production dial.
func parseDurationEnv(name string, logger *slog.Logger) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		logger.Warn("ignoring malformed duration env var",
			"name", name, "value", raw, "err", err)
		return 0
	}
	return d
}
