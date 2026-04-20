package openclaw

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// ActivePollInterval is the cadence used when the gateway is alive.
// Each tick hits the gateway's own HTTP /health endpoint (~10 ms), so
// the budget is dominated by latency rather than spawn cost. One second
// is tight enough that the UI reacts within a tick of a crash while
// still staying well under the /health handler's single-digit-ms cost.
const ActivePollInterval = 1 * time.Second

// IdlePollInterval is the cadence used when the gateway is not alive
// (crashed, deliberately stopped, or CLI missing). The HTTP liveness
// probe is cheap and the recovery window matters more here than the
// baseline load, so we only back off to 2s.
const IdlePollInterval = 2 * time.Second

// DefaultPollInterval is kept as an alias for callers that passed it
// explicitly. New code should rely on the adaptive behaviour.
const DefaultPollInterval = ActivePollInterval

// AutohealConfig controls the opt-in self-heal policy. When Enabled
// is true and every gating condition in Manager.shouldAutoheal
// agrees, the poller fires Trigger in a background goroutine once
// the gateway has been observed !alive for Threshold consecutive
// probes, then sits out Cooldown before it may fire again.
//
// Trigger is invoked with a fresh context carrying a 90 s deadline
// (matching the Fix HTTP handler budget). A nil Trigger turns
// autoheal into a decision-only no-op, which tests use to avoid
// spawning real CLI commands. Production wires Trigger to
// Cascade(ctx, m, discardCh).
type AutohealConfig struct {
	Enabled   bool
	Threshold int
	Cooldown  time.Duration
	Trigger   func(ctx context.Context)
}

// DefaultAutohealThreshold is the default consecutive-!alive count
// before autoheal fires. With IdlePollInterval at 2 s this gives a
// ~6 s confirmation window — long enough to filter a one-tick flap,
// short enough that a real crash recovers before the operator
// switches to another tab.
const DefaultAutohealThreshold = 3

// DefaultAutohealCooldown is the minimum gap between two autoheal
// triggers. Picked to match the rough budget of a Cascade run
// (doctor ≤ 30 s + gateway restart ≤ 15 s + OS restart ≤ 10 s) with
// slack for Node warm-up. A too-short cooldown turns a broken
// gateway into a cascade-spam loop that starves the operator's own
// clicks.
const DefaultAutohealCooldown = 5 * time.Minute

// Manager owns the background poller and the Snapshot store. One
// Manager per process. It is safe to Start once and call Get any
// number of times concurrently.
type Manager struct {
	runner   Runner
	store    *store
	interval time.Duration
	logger   *slog.Logger
	// stateDir is consulted by Discover to read the operator override
	// file. Empty means "no override lookup" — the rest of the
	// discovery chain still works fine without it.
	stateDir string
	// autoheal holds the resolved opt-in self-heal policy. Zero value
	// (Enabled=false) is the default — identical to the pre-autoheal
	// behaviour.
	autoheal AutohealConfig
	// currentAction holds the name ("start"/"stop"/"restart"/"doctor"/
	// "fix") of the user-initiated command currently executing, or ""
	// when idle. Two consumers read it:
	//
	//   - Autoheal gate: non-empty means "operator is driving, stand
	//     down" — identical semantics to the previous actionInFlight
	//     bool, preserved across this refactor.
	//   - ComputePhase: membership in startingActions (start/restart/
	//     fix only) drives PhaseStarting. Stop and doctor must not
	//     promote the badge to "coming up" — stop is already
	//     represented by Intent=stopped, and doctor is read-only.
	//
	// atomic.Pointer over atomic.Value because the latter panics on
	// type mismatch; Pointer[string] is typed end-to-end.
	currentAction atomic.Pointer[string]
	// lastStartAttemptAt is the UnixNano stamp of the most recent
	// successful start/restart/fix CLI completion. ComputePhase uses
	// it to hold PhaseStarting for WarmupGracePeriod after the command
	// returns, covering the gap between CLI ok and the first alive
	// probe landing (typically 1–3 s on macOS launchd cold start, up
	// to 15 s on a cold Node process). Zero = never attempted.
	lastStartAttemptAt atomic.Int64
}

// NewManager constructs a Manager. A nil logger defaults to
// slog.Default. A zero interval defaults to ActivePollInterval; when
// the gateway is not alive the poller backs off to IdlePollInterval
// regardless of the configured active interval.
func NewManager(runner Runner, interval time.Duration, logger *slog.Logger) *Manager {
	if interval <= 0 {
		interval = ActivePollInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		runner:   runner,
		store:    &store{},
		interval: interval,
		logger:   logger,
	}
}

// WithStateDir enables the <stateDir>/openclaw.path operator override.
// Returns m so callers can chain at construction time. Passing an
// empty string is a no-op (discovery falls back to env + PATH + bare
// probes, which is the legacy behaviour).
func (m *Manager) WithStateDir(stateDir string) *Manager {
	m.stateDir = stateDir
	return m
}

// WithAutoheal installs the opt-in self-heal policy. Returns m so
// callers can chain at construction time. Zero-value or Enabled=false
// is explicitly allowed and turns autoheal into a no-op; a missing
// Threshold falls back to DefaultAutohealThreshold and a missing
// Cooldown to DefaultAutohealCooldown so callers only need to set the
// fields they actually want to override.
func (m *Manager) WithAutoheal(cfg AutohealConfig) *Manager {
	if cfg.Threshold <= 0 {
		cfg.Threshold = DefaultAutohealThreshold
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = DefaultAutohealCooldown
	}
	m.autoheal = cfg
	return m
}

// resolveRunner copies m.runner and fills Binary via Discover when the
// caller-supplied runner left it empty. Tests keep passing Runner with
// an explicit Binary (fake shell scripts) and bypass discovery
// entirely — the override check below preserves that path.
func (m *Manager) resolveRunner() (Runner, Discovery) {
	disc := Discovery{Source: SourceNone}
	r := m.runner
	if r.Binary != "" {
		return r, disc
	}
	disc = Discover(m.stateDir)
	if disc.Path != "" {
		r.Binary = disc.Path
	}
	return r, disc
}

// Get returns the latest Snapshot. Safe for concurrent use; the
// returned value is a copy.
func (m *Manager) Get() Snapshot { return m.store.Get() }

// Runner exposes the resolved Runner so callers (notably the Fix
// handler) can spawn cascade commands against the same binary the
// poller probes with. The Binary field reflects whatever Discover
// last resolved to, so external spawns do not re-run discovery and
// cannot disagree with the poller about which openclaw to drive.
// Returned by value.
func (m *Manager) Runner() Runner {
	r, _ := m.resolveRunner()
	return r
}

// ProbeNow runs one synchronous probe and updates the store. Used by
// the Fix handler to refresh state immediately after a recovery step
// so the UI does not have to wait for the next tick. Errors are
// absorbed into the Snapshot; ProbeNow itself never fails.
//
// Intent handling note: probe() copies prev into next at the top so
// Intent survives a successful probe. But a concurrent SetIntent can
// land while probe() is blocked on the CLI (2–3s), and if we write
// `next` as-is we'd clobber it with the stale Intent read at the
// start of probe. So at store-set time we re-read the store's
// current Intent and preserve that, not the Intent from prev.
func (m *Manager) ProbeNow(ctx context.Context) Snapshot {
	prev := m.store.Get()
	runner, disc := m.resolveRunner()
	next := probe(ctx, runner, prev)
	// Surface the resolved binary on the wire regardless of probe
	// outcome — "we looked but found nothing" is as useful to the UI
	// as a successful resolution, and probe() cannot know the path
	// without wiring discovery through its own signature.
	next.BinaryPath = disc.Path
	next.BinarySource = string(disc.Source)
	sample := ProbeSample{
		At:      next.LastProbeAt,
		Alive:   next.Alive,
		ProbeMS: next.LastProbeMS,
	}
	now := time.Now().UTC()
	// fireAutoheal signals the post-lock goroutine dispatch; populated
	// from inside the store.set closure while we hold the lock and the
	// decision is race-free.
	var (
		fireAutoheal      bool
		fireAutohealCount int
		fireCooldownUntil string
	)
	m.store.set(func(s *Snapshot) {
		// Preserve cumulative state that probe() cannot see: Intent
		// (written out-of-band by SetIntent), CrashCount, ProbeHistory,
		// GatewayPIDSince, and the autoheal bookkeeping fields. Reading
		// from s (the lock-held current snapshot) rather than prev
		// closes the gap where a concurrent writer lands between Get()
		// above and set() here.
		intent := s.Intent
		crashes := s.CrashCount
		history := s.ProbeHistory
		pidSince := s.GatewayPIDSince
		prevPID := s.GatewayPID
		prevAlive := s.Alive
		hadProbed := s.Probed
		ahCount := s.AutohealCount
		ahLastAt := s.AutohealLastAt
		ahNextAt := s.AutohealNextEligibleAt
		ahNext := s.AutohealNextEligible
		ahConsec := s.AutohealConsecutiveDown
		*s = next
		s.Intent = intent

		// CrashCount increments on every observed alive=true→false
		// edge. `hadProbed` guards the first tick after boot so an
		// initial alive=false does not count as a crash (we never saw
		// it up in the first place).
		if hadProbed && prevAlive && !next.Alive {
			crashes++
		}
		s.CrashCount = crashes

		// GatewayPIDSince resets when we observe a different non-zero
		// PID. Keeping it stable across a momentary PID=0 (status flap
		// during a transient) means uptime does not reset every time
		// `gateway status` catches the service mid-teardown.
		switch {
		case next.GatewayPID != 0 && next.GatewayPID != prevPID:
			s.GatewayPIDSince = rfc3339(now)
		case next.GatewayPID == 0:
			// Keep whatever we had; a zero PID could be transient.
			s.GatewayPIDSince = pidSince
		default:
			s.GatewayPIDSince = pidSince
		}

		s.ProbeHistory = appendProbeSample(history, sample)

		// Restore autoheal bookkeeping that *s = next wiped. The
		// consecutive-down counter advances or resets based on the
		// just-observed Alive bit so the gating check below sees the
		// current streak length.
		s.AutohealCount = ahCount
		s.AutohealLastAt = ahLastAt
		s.AutohealNextEligibleAt = ahNextAt
		s.AutohealNextEligible = ahNext
		s.AutohealConsecutiveDown = ahConsec
		s.AutohealEnabled = m.autoheal.Enabled
		// While a user action is in flight, force the consecutive-down
		// counter to zero. The gateway is legitimately !alive during
		// the first few seconds of `openclaw gateway start`; counting
		// those ticks would accumulate toward the threshold and fire
		// autoheal the moment the action returned, spawning a second
		// cascade that races with the user's request.
		curAction := m.CurrentAction()
		actionLive := curAction != ""
		switch {
		case actionLive:
			s.AutohealConsecutiveDown = 0
		case next.Alive:
			s.AutohealConsecutiveDown = 0
		default:
			s.AutohealConsecutiveDown++
		}

		// Phase is computed inside the lock so it observes the exact
		// Snapshot being stored, not a race-risk "get + recompute"
		// pair. The UI reads s.Phase directly and must not re-derive
		// the same decision (phase.go's comment explains why).
		s.Phase = ComputePhase(*s, curAction, m.LastStartAttempt(), now)

		// Autoheal decision runs inside the lock so two concurrent
		// ProbeNow calls cannot both observe "at threshold" and fire
		// twice: the second caller will see the reset counter the
		// first caller wrote. The action-in-flight gate above also
		// keeps shouldAutoheal returning false here even in the edge
		// case where a stale counter reached threshold moments before
		// the user pressed a button.
		if !actionLive && shouldAutoheal(s, m.autoheal, now) {
			s.AutohealCount++
			s.AutohealLastAt = rfc3339(now)
			s.AutohealNextEligible = now.Add(m.autoheal.Cooldown)
			s.AutohealNextEligibleAt = rfc3339(s.AutohealNextEligible)
			s.AutohealConsecutiveDown = 0
			fireAutoheal = true
			fireAutohealCount = s.AutohealCount
			fireCooldownUntil = s.AutohealNextEligibleAt
		}
	})
	if fireAutoheal {
		m.logger.Info("openclaw: autoheal triggered",
			"component", "openclaw",
			"count", fireAutohealCount,
			"cooldown_until", fireCooldownUntil)
		if m.autoheal.Trigger != nil {
			go func() {
				tctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()
				m.autoheal.Trigger(tctx)
			}()
		}
	}
	return m.store.Get()
}

// appendProbeSample appends sample to history, allocating a new slice
// to avoid aliasing the caller's backing array, and trims the result
// to the most recent ProbeHistoryLen entries.
func appendProbeSample(history []ProbeSample, sample ProbeSample) []ProbeSample {
	out := make([]ProbeSample, 0, len(history)+1)
	out = append(out, history...)
	out = append(out, sample)
	if len(out) > ProbeHistoryLen {
		out = out[len(out)-ProbeHistoryLen:]
	}
	return out
}

// SetIntent records the operator's stated desire for the gateway's
// run state. Called by the HTTP handlers before dispatching an action
// so the UI can distinguish "user stopped it" (已停止) from "it
// crashed" (异常) when the next probe reports !alive. Doctor does not
// call this — it is read-only.
func (m *Manager) SetIntent(intent Intent) {
	m.store.set(func(s *Snapshot) { s.Intent = intent })
}

// setCurrentAction records the name of the action now running so
// autoheal can stand down and ComputePhase can elect PhaseStarting.
// RunAction and the Fix handler call this at the top and pair it
// with a deferred clearCurrentAction so a panic cannot leave the
// flag stuck. Empty name clears the slot (same as
// clearCurrentAction, kept as a convenience for tests).
func (m *Manager) setCurrentAction(name string) {
	if name == "" {
		m.currentAction.Store(nil)
		return
	}
	m.currentAction.Store(&name)
}

// clearCurrentAction resets the in-flight slot to empty.
func (m *Manager) clearCurrentAction() { m.currentAction.Store(nil) }

// CurrentAction returns the name of the currently executing action,
// or "" when idle. Exported so tests and (future) diagnostic handlers
// can observe the gate without reaching into the atomic.
func (m *Manager) CurrentAction() string {
	if p := m.currentAction.Load(); p != nil {
		return *p
	}
	return ""
}

// stampStartAttempt records now as the most recent start/restart/fix
// attempt completion. Used by ComputePhase to hold PhaseStarting
// through the WarmupGracePeriod window. Called unconditionally for
// start/restart (even on non-zero CLI exit: see action.go's comment
// on why CLI self-check exit codes are unreliable) and conditionally
// for cascade tiers that actually repaired the gateway. Stop and
// doctor do not call this — their post-action state is conveyed by
// Intent and (for doctor) by the unchanged gateway state.
func (m *Manager) stampStartAttempt(now time.Time) {
	m.lastStartAttemptAt.Store(now.UnixNano())
}

// LastStartAttempt returns the time of the most recent stamped
// attempt, or the zero time when none has been recorded. Exported
// so ComputePhase callers in tests can inject expected values.
func (m *Manager) LastStartAttempt() time.Time {
	ns := m.lastStartAttemptAt.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

// Start runs the poll loop until ctx is cancelled. It probes once
// immediately so /api/openclaw/status returns real data within a
// second of boot, then schedules the next probe adaptively: the
// configured interval (default 3s) while alive, IdlePollInterval (5s)
// while not. Start blocks; callers launch it in a goroutine.
//
// A second goroutine resolves the gateway address from
// `openclaw gateway status --json` and refreshes it every
// AddressRefreshInterval. The resolver is decoupled from the probe
// loop because Node spawn cost (~2–3 s) must not block the 1-second
// liveness cadence. The first resolution races with the first probe:
// until it lands, probes hit the historical default
// DefaultGatewayHost:DefaultGatewayPort, which is correct for every
// out-of-the-box install.
func (m *Manager) Start(ctx context.Context) {
	go m.runAddressResolver(ctx)
	m.ProbeNow(ctx)
	for {
		d := m.interval
		if !m.store.Get().Alive {
			d = IdlePollInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
			m.ProbeNow(ctx)
		}
	}
}

// runAddressResolver keeps the shared gateway-address cache current.
// Runs one resolution immediately (so a port override lands before
// the second probe tick) and then re-resolves every
// AddressRefreshInterval. Errors are logged at debug — a failed
// resolution leaves the previous (or seed) address serving probes,
// which is the correct fail-soft behaviour.
func (m *Manager) runAddressResolver(ctx context.Context) {
	m.resolveOnce(ctx)
	t := time.NewTicker(AddressRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.resolveOnce(ctx)
		}
	}
}

func (m *Manager) resolveOnce(ctx context.Context) {
	runner, _ := m.resolveRunner()
	if runner.Binary == "" {
		return
	}
	addr, err := ResolveGatewayAddress(ctx, runner)
	if err != nil {
		m.logger.Debug("openclaw: gateway address resolve failed",
			slog.String("err", err.Error()))
		return
	}
	m.logger.Debug("openclaw: gateway address resolved",
		slog.String("host", addr.Host),
		slog.Int("port", addr.Port),
		slog.String("source", addr.Source))
}
