package openclaw

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// DefaultBinary is the executable name resolved on PATH. Tests and
// wrappers can override via Runner.Binary.
const DefaultBinary = "openclaw"

// Runner wraps exec.CommandContext so tests can stub the binary without
// touching the filesystem. The zero value uses DefaultBinary on PATH
// and no env overrides.
type Runner struct {
	// Binary overrides the executable. Empty falls back to PATH
	// lookup of DefaultBinary.
	Binary string
	// Env, when non-nil, replaces the child's environment. Nil
	// inherits the parent process environment (the normal case).
	Env []string
}

// Result captures one CLI spawn outcome. Stdout/Stderr are captured
// as bytes so JSON callers can json.Unmarshal without a re-read and
// human callers can forward the text to the UI.
type Result struct {
	Argv     []string
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Duration time.Duration
	// Killed is true when the context fired before the process exited
	// and we had to send SIGKILL. Upstream bug #11843 trips this path
	// even when the command finished its useful work.
	Killed bool
}

// ErrBinaryMissing is returned by runCmd when the openclaw executable
// cannot be located on PATH. Callers map it to a friendly
// "openclaw not installed" rendering.
var ErrBinaryMissing = errors.New("openclaw: binary not found on PATH")

// runCmd spawns the openclaw CLI with the given arguments and waits up
// to timeout for completion. Exceeding timeout returns the partial
// output with Killed=true. A non-zero exit code does not return an
// error; the caller decides whether the exit code is failure-enough to
// surface to the user (many CLIs use 1 to signal "gateway down" which
// is useful information, not a hard error).
func (r Runner) runCmd(ctx context.Context, timeout time.Duration, args ...string) (*Result, error) {
	bin := r.Binary
	if bin == "" {
		bin = DefaultBinary
	}
	if _, err := exec.LookPath(bin); err != nil && r.Binary == "" {
		return nil, ErrBinaryMissing
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, bin, args...)
	// CommandContext's default kill is SIGKILL on Unix, which is what
	// we want for #11843 hangs — no graceful-TERM window, no chance
	// to linger. Windows has no real SIGKILL; TerminateProcess is the
	// closest match and is what Go already does on that platform.
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	// cmd.Cancel SIGKILLs the direct child, but a shell-wrapped fake
	// CLI (tests) or a real CLI that has already forked a subprocess
	// can leave grandchildren holding stdout/stderr. Go's cmd.Run
	// blocks until those pipes close, which on a busy -race runner
	// can be the full remaining sleep of a hung grandchild (hundreds
	// of seconds in the worst case). WaitDelay bounds that tail:
	// after Cancel fires, I/O is forcibly torn down at +500ms so
	// Run returns promptly. Callers still see DeadlineExceeded via
	// cctx, so outcome reporting is unchanged.
	cmd.WaitDelay = 500 * time.Millisecond
	if r.Env != nil {
		cmd.Env = r.Env
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start)

	res := &Result{
		Argv:     append([]string{bin}, args...),
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		Duration: dur,
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		res.Killed = true
		return res, fmt.Errorf("openclaw %v: timeout after %s (killed)", args, timeout)
	}
	if err != nil {
		// exec.ExitError is expected for non-zero exit codes; fold
		// it into the Result and return nil so the caller can decide.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return res, nil
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			res.Killed = true
			return res, ctx.Err()
		}
		return res, fmt.Errorf("openclaw %v: %w", args, err)
	}
	return res, nil
}
