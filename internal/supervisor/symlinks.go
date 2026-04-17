//go:build unix

package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNoRollbackTarget is returned by SwapCurrentPrevious when there
// is no distinct previous version to roll back to. Callers transition
// the supervisor to Stopped and exit 65 per
// architecture/supervisor-protocol.md §6.3 step 2.
var ErrNoRollbackTarget = errors.New("supervisor: no distinct previous version to roll back to")

// CurrentLink returns the absolute path of the "current" symlink
// under root. It does not verify the target exists.
func CurrentLink(root string) string { return filepath.Join(root, "current") }

// PreviousLink returns the absolute path of the "previous" symlink
// under root.
func PreviousLink(root string) string { return filepath.Join(root, "previous") }

// SwapCurrentPrevious atomically swaps the current and previous
// symlinks under root. The swap is a pair of renames through a
// temporary name so neither link ever vanishes for longer than a
// single syscall window:
//
//	rename(current,  tmp)
//	rename(previous, current)
//	rename(tmp,      previous)
//
// The implementation fails closed: if any step fails the function
// returns an error and callers should treat the install root as
// possibly inconsistent and escalate to manual recovery.
func SwapCurrentPrevious(root string) error {
	if root == "" {
		return ErrNoRollbackTarget
	}
	cur := CurrentLink(root)
	prev := PreviousLink(root)

	curTarget, err := os.Readlink(cur)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNoRollbackTarget
		}
		return fmt.Errorf("supervisor: read current symlink: %w", err)
	}
	prevTarget, err := os.Readlink(prev)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNoRollbackTarget
		}
		return fmt.Errorf("supervisor: read previous symlink: %w", err)
	}
	if curTarget == prevTarget {
		return ErrNoRollbackTarget
	}

	tmp := filepath.Join(root, ".swap.tmp")
	// Clean up any stray tmp link from a prior crashed swap before
	// we proceed. Missing is the normal case.
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("supervisor: clear swap tmp: %w", err)
	}

	if err := os.Rename(cur, tmp); err != nil {
		return fmt.Errorf("supervisor: swap step 1: %w", err)
	}
	if err := os.Rename(prev, cur); err != nil {
		// Best-effort: try to restore current before surfacing.
		_ = os.Rename(tmp, cur)
		return fmt.Errorf("supervisor: swap step 2: %w", err)
	}
	if err := os.Rename(tmp, prev); err != nil {
		// Current is now the old-previous, so we have swapped but
		// lost the previous pointer. Surface the error so an
		// operator can repair state/; we do not try to undo.
		return fmt.Errorf("supervisor: swap step 3: %w", err)
	}
	return nil
}

// ResolveWorkerBinary returns the absolute path of the worker binary
// the supervisor should spawn: <root>/current/<binaryName>. It
// follows the symlink so exec picks up the rolled-back target
// immediately after a swap.
func ResolveWorkerBinary(root, binaryName string) (string, error) {
	cur := CurrentLink(root)
	target, err := os.Readlink(cur)
	if err != nil {
		return "", fmt.Errorf("supervisor: resolve current: %w", err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	return filepath.Join(target, binaryName), nil
}
