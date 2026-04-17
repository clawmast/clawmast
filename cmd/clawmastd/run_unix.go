//go:build unix

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/clawmast/clawmast/internal/supervisor"
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
		InstallRoot: opts.installRoot,
		Logger:      logger,
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
