//go:build unix

package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// session is the supervisor-side handle on one spawned worker
// process. It owns the child until Wait returns; callers interact
// with it through Start, signalling, and Wait.
type session struct {
	cmd       *exec.Cmd
	path      string
	version   string
	args      []string
	stopGrace time.Duration
	// waitCh delivers the exit of cmd.Wait() exactly once.
	waitCh chan waitResult
}

// waitResult is the outcome of cmd.Wait(). ExitInfo is pre-extracted
// on the reaping goroutine so the main loop sees a value type.
type waitResult struct {
	Info ExitInfo
	Err  error
}

// spawn starts the worker with the notify environment baked in and
// the child in its own process group so the supervisor can forward
// signals selectively (protocol §7). It returns once the child has
// been exec'd; failures from cmd.Start are returned directly.
//
// The child is intentionally not attached to a context: the
// supervisor owns the SIGTERM→grace→SIGKILL sequence via
// session.stop, and exec.CommandContext's implicit SIGKILL would
// cut that short.
func spawn(cfg Config, path, version string) (*session, error) {
	cmd := exec.Command(path, cfg.WorkerArgs...)
	cmd.Stdout = cfg.Stdout
	cmd.Stderr = cfg.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	env := append([]string{}, os.Environ()...)
	env = append(env, cfg.ExtraEnv...)
	env = append(env,
		"NOTIFY_SOCKET="+cfg.NotifySocketPath,
		"WATCHDOG_USEC="+strconv.FormatInt(cfg.WatchdogInterval.Microseconds(), 10),
		// CLAWMAST_VERSION_LABEL carries the supervisor's notion of the
		// running version — the name of the directory the "current"
		// symlink resolves to. It may differ from the binary's
		// compile-time version.Version (e.g. local dev builds reporting
		// "abc123-dirty") and is the label the worker must use when
		// writing to state/blacklist.json so the supervisor's pre-spawn
		// check matches.
		"CLAWMAST_VERSION_LABEL="+version,
	)
	// WATCHDOG_PID is optional in the protocol (§5.1). We omit it
	// in v1.0: the only way to inject it at exec time is to know
	// the child pid before fork, which requires custom fork/exec
	// plumbing we do not need yet.
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("supervisor: spawn %s: %w", path, err)
	}

	s := &session{
		cmd:       cmd,
		path:      path,
		version:   version,
		args:      cfg.WorkerArgs,
		stopGrace: cfg.StopGrace,
		waitCh:    make(chan waitResult, 1),
	}
	go func() {
		err := cmd.Wait()
		s.waitCh <- waitResult{Info: FromProcessState(cmd.ProcessState), Err: err}
		close(s.waitCh)
	}()
	return s, nil
}

// Pid returns the child pid. Safe to call after spawn returns.
func (s *session) Pid() int { return s.cmd.Process.Pid }

// signal forwards a signal to the child. It is a no-op if the child
// has already been reaped.
func (s *session) signal(sig os.Signal) {
	if s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Signal(sig)
}

// stop drives the Stopping sequence on this session: SIGTERM, wait
// up to StopGrace for reap, then SIGKILL and wait once more. Returns
// the exit result as reported by cmd.Wait().
func (s *session) stop(ctx context.Context) waitResult {
	s.signal(syscall.SIGTERM)
	timer := time.NewTimer(s.stopGrace)
	defer timer.Stop()
	select {
	case r := <-s.waitCh:
		return r
	case <-timer.C:
	case <-ctx.Done():
	}
	// Grace expired — force kill and wait unconditionally. At this
	// point cmd.Wait has not returned, so the waitCh is still open.
	_ = s.cmd.Process.Kill()
	return <-s.waitCh
}

// WritePidFile persists the child pid under runDir/worker.pid. A
// failure is logged by the caller, not returned, because the
// supervisor can still function without the convenience file.
func (s *session) WritePidFile(runDir string) error {
	path := filepath.Join(runDir, "worker.pid")
	data := []byte(strconv.Itoa(s.cmd.Process.Pid) + "\n")
	return os.WriteFile(path, data, 0o600)
}

// clearPidFile removes runDir/worker.pid. Errors are ignored because
// the file may already be gone after a stop.
func clearPidFile(runDir string) {
	_ = os.Remove(filepath.Join(runDir, "worker.pid"))
}
