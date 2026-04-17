//go:build unix

package supervisor

import (
	"os"
	"syscall"
)

// Classification categorises a reaped worker exit per
// architecture/supervisor-protocol.md §4. The main loop combines the
// classification with its own state to decide the next transition
// (notably demoting a spontaneous zero exit during Running to
// ClassUnexpected; see protocol §4 edge rules).
type Classification int

const (
	// ClassGraceful is a clean exit 0 from the worker. The caller is
	// responsible for deciding whether it is a true graceful exit
	// (only when the supervisor is already in Stopping) or a
	// spontaneous zero that must be demoted to ClassUnexpected.
	ClassGraceful Classification = iota
	// ClassUnexpected covers codes 1–63, any reserved code in
	// 66–127, and signal-killed terminations. Triggers Restarting.
	ClassUnexpected
	// ClassNoRestart is exit 64: the worker explicitly asks the
	// supervisor to stop without respawning.
	ClassNoRestart
	// ClassRollback is exit 65: the worker declares the current
	// version defective and asks for a symlink swap.
	ClassRollback
)

// String returns a short lower-case label suitable for log fields and
// the on-disk history ledger.
func (c Classification) String() string {
	switch c {
	case ClassGraceful:
		return "graceful"
	case ClassNoRestart:
		return "no-restart"
	case ClassRollback:
		return "rollback"
	default:
		return "unexpected"
	}
}

// ExitInfo is the platform-neutral view of a reaped process the
// supervisor cares about. It is produced by FromProcessState and
// consumed by ClassifyExitCode so the classification logic stays a
// pure function amenable to table-driven tests.
type ExitInfo struct {
	// Code is the POSIX exit status (0–255). When Signaled is true
	// this holds the signal number so history entries still carry a
	// useful integer; callers must not use Code to drive policy when
	// Signaled is true — use Classify, which ignores Code in that
	// case.
	Code int
	// Signaled is true when the worker was terminated by a signal
	// rather than calling exit(2). Per protocol §4 this is always
	// ClassUnexpected.
	Signaled bool
}

// FromProcessState extracts the platform-neutral ExitInfo from a
// reaped os.ProcessState. It handles the POSIX convention where
// signal-killed processes report ExitCode()==-1 and expose the signal
// via syscall.WaitStatus.
func FromProcessState(st *os.ProcessState) ExitInfo {
	if st == nil {
		return ExitInfo{Code: -1, Signaled: false}
	}
	if ws, ok := st.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return ExitInfo{Code: int(ws.Signal()), Signaled: true}
		}
		return ExitInfo{Code: ws.ExitStatus(), Signaled: false}
	}
	// Fallback for platforms where Sys() is not WaitStatus. Should
	// not happen under //go:build unix but keeps the function total.
	return ExitInfo{Code: st.ExitCode(), Signaled: false}
}

// ClassifyExitCode implements the normative table in
// supervisor-protocol.md §4. It never returns a demoted-graceful
// result: the caller must apply the edge rule for spontaneous zero
// exits during Running itself, because the classification function
// has no view of supervisor state.
func ClassifyExitCode(info ExitInfo) Classification {
	if info.Signaled {
		return ClassUnexpected
	}
	switch {
	case info.Code == 0:
		return ClassGraceful
	case info.Code == 64:
		return ClassNoRestart
	case info.Code == 65:
		return ClassRollback
	case info.Code >= 1 && info.Code <= 63:
		return ClassUnexpected
	default:
		// 66–127 reserved for future extensions; 128+N already
		// normalised via Signaled above; anything else (including
		// negative or >127) is treated as unexpected.
		return ClassUnexpected
	}
}
