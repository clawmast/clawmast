package openclaw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ProbeTimeout bounds one `openclaw health --json` spawn. Upstream
// advertises a 10s internal timeout (docs/cli/health.md), so we allow
// a small margin above that before SIGKILLing for a #11843 hang.
const ProbeTimeout = 12 * time.Second

// probe runs one health check and returns an updated Snapshot built
// from the previous one (so sticky fields like LastAliveAt are
// preserved across transient failures). probe never panics and always
// returns a fully-populated Snapshot — errors are recorded in
// Snapshot.ProbeError, not returned.
func probe(ctx context.Context, runner Runner, prev Snapshot) Snapshot {
	next := prev
	next.Probed = true
	now := time.Now().UTC()
	next.LastProbeAt = rfc3339(now)

	res, err := runner.runCmd(ctx, ProbeTimeout, "health", "--json")
	if errors.Is(err, ErrBinaryMissing) {
		next.CLIMissing = true
		next.Alive = false
		next.ProbeError = "openclaw binary not found on PATH"
		next.LastProbeMS = 0
		return next
	}
	next.CLIMissing = false
	if res != nil {
		next.LastProbeMS = res.Duration.Milliseconds()
	}
	if err != nil {
		next.Alive = false
		next.ProbeError = err.Error()
		return next
	}
	if res.ExitCode != 0 {
		next.Alive = false
		// Surface the tail of stderr so the UI can render something
		// more actionable than "exit 1". Capping at 400 bytes keeps
		// response size bounded.
		next.ProbeError = fmt.Sprintf("exit %d: %s", res.ExitCode, trimErr(res.Stderr))
		return next
	}
	var payload healthPayload
	if err := json.Unmarshal(res.Stdout, &payload); err != nil {
		next.Alive = false
		next.ProbeError = fmt.Sprintf("parse health json: %v", err)
		return next
	}
	if !payload.OK {
		next.Alive = false
		next.ProbeError = "gateway reports ok=false"
		return next
	}
	next.Alive = true
	next.ProbeError = ""
	next.LastAliveAt = rfc3339(now)
	next.HealthTS = payload.TS
	next.HealthDurationMS = payload.DurationMS
	next.DefaultAgentID = payload.DefaultAgentID
	next.HeartbeatSeconds = payload.HeartbeatSeconds
	next.SessionsCount = payload.Sessions.Count
	next.ChannelCount = len(payload.Channels)
	return next
}

// trimErr keeps the last ~400 bytes of stderr, trimmed. The tail is
// usually where Node prints the actual error after a stack trace.
func trimErr(b []byte) string {
	const cap = 400
	if len(b) <= cap {
		return strings.TrimSpace(string(b))
	}
	return strings.TrimSpace(string(b[len(b)-cap:]))
}
