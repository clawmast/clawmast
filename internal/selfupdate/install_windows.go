//go:build windows

package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"runtime"
)

// Apply on Windows is a documented no-op that fails with a wrapped
// [errors.ErrUnsupported]. Auto-update is a Tier-1 (macOS / Linux)
// feature per refactor.md decision #2; Windows worker installs are
// manual-only until Tier-1 parity lands post-v1.0.
//
// The stub exists so cmd/clawmast and internal/agent keep building
// on windows/amd64 (AGENTS.md R5) while referencing [Apply]
// unconditionally from shared code paths.
func Apply(_ context.Context, _ Config) (Result, error) {
	return Result{}, fmt.Errorf("selfupdate: %s/%s: %w",
		runtime.GOOS, runtime.GOARCH, errors.ErrUnsupported)
}
