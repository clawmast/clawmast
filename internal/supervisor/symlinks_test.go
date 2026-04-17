//go:build unix

package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// layout builds a minimal install root with two version directories
// and the requested current/previous targets. It returns the root.
func layout(t *testing.T, current, previous string) string {
	t.Helper()
	root := t.TempDir()
	for _, v := range []string{"v0.1.0", "v0.2.0"} {
		if err := os.MkdirAll(filepath.Join(root, "versions", v), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if current != "" {
		if err := os.Symlink(filepath.Join("versions", current), filepath.Join(root, "current")); err != nil {
			t.Fatalf("symlink current: %v", err)
		}
	}
	if previous != "" {
		if err := os.Symlink(filepath.Join("versions", previous), filepath.Join(root, "previous")); err != nil {
			t.Fatalf("symlink previous: %v", err)
		}
	}
	return root
}

func TestSwapCurrentPrevious(t *testing.T) {
	root := layout(t, "v0.2.0", "v0.1.0")
	if err := SwapCurrentPrevious(root); err != nil {
		t.Fatalf("SwapCurrentPrevious: %v", err)
	}
	curT, _ := os.Readlink(filepath.Join(root, "current"))
	prevT, _ := os.Readlink(filepath.Join(root, "previous"))
	if curT != filepath.Join("versions", "v0.1.0") {
		t.Errorf("current now = %q", curT)
	}
	if prevT != filepath.Join("versions", "v0.2.0") {
		t.Errorf("previous now = %q", prevT)
	}
}

func TestSwapCurrentPreviousMissingPrev(t *testing.T) {
	root := layout(t, "v0.2.0", "")
	err := SwapCurrentPrevious(root)
	if !errors.Is(err, ErrNoRollbackTarget) {
		t.Fatalf("want ErrNoRollbackTarget, got %v", err)
	}
}

func TestSwapCurrentPreviousSameTarget(t *testing.T) {
	root := layout(t, "v0.2.0", "v0.2.0")
	err := SwapCurrentPrevious(root)
	if !errors.Is(err, ErrNoRollbackTarget) {
		t.Fatalf("want ErrNoRollbackTarget, got %v", err)
	}
}

func TestSwapClearsStaleTmp(t *testing.T) {
	root := layout(t, "v0.2.0", "v0.1.0")
	tmp := filepath.Join(root, ".swap.tmp")
	if err := os.Symlink("versions/orphan", tmp); err != nil {
		t.Fatalf("seed stale tmp: %v", err)
	}
	if err := SwapCurrentPrevious(root); err != nil {
		t.Fatalf("SwapCurrentPrevious: %v", err)
	}
	if _, err := os.Lstat(tmp); !os.IsNotExist(err) {
		t.Fatalf("expected tmp removed, stat err=%v", err)
	}
}

func TestResolveWorkerBinary(t *testing.T) {
	root := layout(t, "v0.2.0", "v0.1.0")
	got, err := ResolveWorkerBinary(root, "clawmast")
	if err != nil {
		t.Fatalf("ResolveWorkerBinary: %v", err)
	}
	want := filepath.Join(root, "versions", "v0.2.0", "clawmast")
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
