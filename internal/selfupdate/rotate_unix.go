//go:build unix

package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
)

// rotateSymlinks updates <root>/current and <root>/previous so that
// `current` points at the newly-installed version and `previous`
// preserves whatever `current` referred to beforehand. The operation
// mirrors what scripts/install.sh swap_current does on a fresh shell
// install, so a mixed-mode install tree (first install via install.sh,
// subsequent updates via this package) stays coherent.
//
// Returns (before, after, previous) where:
//
//   - before: target of `current` before the call, or "" on fresh install.
//   - after:  target of `current` after the call (always
//     "versions/<version>").
//   - previous: target of `previous` after the call (the prior current,
//     or "" on fresh install).
//
// The function is deliberately non-atomic in the install-sh sense: it
// updates two links with two syscalls. An operator running this by
// hand under `strace` will see a brief window where `current` points
// at the new version but `previous` still points at the version
// before-last. That is acceptable because:
//
//  1. The supervisor only reads `current` when spawning a new worker,
//     which does not happen during this window.
//  2. The blacklist rollback path uses [supervisor.SwapCurrentPrevious]
//     which is its own 3-rename dance; rotateSymlinks only *prepares*
//     the links so a later swap has a sensible target.
//
// A failure between the two syscalls leaves `current` updated and
// `previous` stale; the caller logs the error and returns it, and the
// operator can run the remaining `ln -sfn` manually. We do not attempt
// recovery because the only recovery would be to undo the first link,
// which reintroduces the race we are avoiding.
func rotateSymlinks(root, version string) (before, after, previous string, err error) {
	cur := filepath.Join(root, "current")
	prev := filepath.Join(root, "previous")
	target := filepath.Join("versions", version)

	// Read the existing current so we can promote it to previous.
	if existing, readErr := os.Readlink(cur); readErr == nil {
		before = existing
		if existing != target {
			// ln -snf: remove then symlink. os.Symlink fails if the
			// link already exists, so we remove first. This matches
			// install.sh behaviour exactly.
			if err := os.Remove(prev); err != nil && !os.IsNotExist(err) {
				return before, "", "", fmt.Errorf("clear previous: %w", err)
			}
			if err := os.Symlink(existing, prev); err != nil {
				return before, "", "", fmt.Errorf("write previous: %w", err)
			}
			previous = existing
		} else {
			// Already pointed at the target; leave previous alone.
			// Read whatever previous happens to be so the caller can
			// report it back to the UI.
			if p, err := os.Readlink(prev); err == nil {
				previous = p
			}
		}
	}

	if err := os.Remove(cur); err != nil && !os.IsNotExist(err) {
		return before, "", previous, fmt.Errorf("clear current: %w", err)
	}
	if err := os.Symlink(target, cur); err != nil {
		return before, "", previous, fmt.Errorf("write current: %w", err)
	}
	after = target
	return before, after, previous, nil
}
