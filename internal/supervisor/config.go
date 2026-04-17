//go:build unix

package supervisor

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Defaults align with architecture/supervisor-protocol.md.
const (
	DefaultStartTimeout     = 30 * time.Second
	DefaultWatchdogInterval = 30 * time.Second
	DefaultStopGrace        = 10 * time.Second
	DefaultWorkerBinaryName = "clawmast"
)

// Config is the public knob surface for Supervisor.Run.
//
// The zero value is invalid: at minimum InstallRoot or a custom
// ResolveWorker must be set. All other fields fall back to
// protocol-specified defaults.
type Config struct {
	// InstallRoot is the ~/.clawmast (or equivalent) directory.
	// When ResolveWorker is nil the supervisor resolves the worker
	// binary as <InstallRoot>/current/<WorkerBinaryName>. The
	// rollback path swaps <InstallRoot>/current with /previous.
	InstallRoot string
	// WorkerBinaryName overrides the "clawmast" default inside the
	// versioned directories.
	WorkerBinaryName string
	// WorkerArgs are appended after the binary path in every spawn.
	WorkerArgs []string
	// ExtraEnv is appended to the spawned worker environment in
	// addition to os.Environ() and the notify variables.
	ExtraEnv []string

	// StartTimeout is the maximum time the supervisor waits for
	// READY=1 in Starting. Default 30 s.
	StartTimeout time.Duration
	// WatchdogInterval is the maximum gap between WATCHDOG=1
	// datagrams in Running. Default 30 s.
	WatchdogInterval time.Duration
	// StopGrace is the SIGTERM→SIGKILL window in Stopping.
	// Default 10 s.
	StopGrace time.Duration

	// Backoff governs restart timing and the crash-loop cap.
	Backoff BackoffPolicy

	// NotifySocketPath overrides the default
	// <InstallRoot>/run/notify.sock location.
	NotifySocketPath string
	// HistoryPath overrides the default
	// <InstallRoot>/state/history.json location.
	HistoryPath string
	// RunDir overrides the default <InstallRoot>/run directory
	// (used for supervisor.pid and worker.pid).
	RunDir string

	// ResolveWorker is an optional hook that fully owns the
	// mapping from supervisor-state to the path + logical version
	// of the binary to exec. Tests use this to bypass the symlink
	// machinery. Nil falls back to
	// ResolveWorkerBinary(InstallRoot, WorkerBinaryName) + the
	// symlink target as the version label.
	ResolveWorker func() (path, version string, err error)

	// Logger captures all operational events. Nil → slog.Default.
	Logger *slog.Logger
	// Stdout and Stderr are attached to the spawned worker. Nil
	// falls back to os.Stdout / os.Stderr so operators see the
	// worker inline when running under launchd / systemd.
	Stdout, Stderr io.Writer
}

// withDefaults returns a copy of c with zero-valued fields populated
// from the protocol-defined defaults.
func (c Config) withDefaults() (Config, error) {
	out := c
	if out.WorkerBinaryName == "" {
		out.WorkerBinaryName = DefaultWorkerBinaryName
	}
	if out.StartTimeout == 0 {
		out.StartTimeout = DefaultStartTimeout
	}
	if out.WatchdogInterval == 0 {
		out.WatchdogInterval = DefaultWatchdogInterval
	}
	if out.StopGrace == 0 {
		out.StopGrace = DefaultStopGrace
	}
	if out.Backoff.Base == 0 {
		out.Backoff = NewBackoff()
	}
	if out.ResolveWorker == nil && out.InstallRoot == "" {
		return out, errors.New("supervisor: Config requires InstallRoot or ResolveWorker")
	}
	if out.InstallRoot != "" {
		if out.NotifySocketPath == "" {
			out.NotifySocketPath = filepath.Join(out.InstallRoot, "run", "notify.sock")
		}
		if out.HistoryPath == "" {
			out.HistoryPath = filepath.Join(out.InstallRoot, "state", "history.json")
		}
		if out.RunDir == "" {
			out.RunDir = filepath.Join(out.InstallRoot, "run")
		}
	}
	if out.NotifySocketPath == "" || out.HistoryPath == "" || out.RunDir == "" {
		return out, errors.New("supervisor: Config needs NotifySocketPath, HistoryPath and RunDir when InstallRoot is empty")
	}
	if out.Logger == nil {
		out.Logger = slog.Default()
	}
	if out.Stdout == nil {
		out.Stdout = os.Stdout
	}
	if out.Stderr == nil {
		out.Stderr = os.Stderr
	}
	if out.ResolveWorker == nil {
		root := out.InstallRoot
		name := out.WorkerBinaryName
		out.ResolveWorker = func() (string, string, error) {
			path, err := ResolveWorkerBinary(root, name)
			if err != nil {
				return "", "", err
			}
			target, _ := os.Readlink(CurrentLink(root))
			return path, filepath.Base(target), nil
		}
	}
	return out, nil
}
