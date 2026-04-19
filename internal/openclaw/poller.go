package openclaw

import (
	"context"
	"log/slog"
	"time"
)

// ActivePollInterval is the cadence used when the gateway is alive.
// 3s catches an unresponsive / crashed gateway quickly (the badge goes
// stale for at most one tick) at the cost of one extra CLI spawn per
// 2s compared to the old 5s.
const ActivePollInterval = 3 * time.Second

// IdlePollInterval is the cadence used when the gateway is not alive
// (crashed, deliberately stopped, or CLI missing). Once down, probing
// every 3s adds no signal — the operator has to take action — so we
// back off to keep Node spawn noise low.
const IdlePollInterval = 5 * time.Second

// DefaultPollInterval is kept as an alias for callers that passed it
// explicitly. New code should rely on the adaptive behaviour.
const DefaultPollInterval = ActivePollInterval

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
	m.store.set(func(s *Snapshot) {
		intent := s.Intent
		*s = next
		s.Intent = intent
	})
	return m.store.Get()
}

// SetIntent records the operator's stated desire for the gateway's
// run state. Called by the HTTP handlers before dispatching an action
// so the UI can distinguish "user stopped it" (已停止) from "it
// crashed" (异常) when the next probe reports !alive. Doctor does not
// call this — it is read-only.
func (m *Manager) SetIntent(intent Intent) {
	m.store.set(func(s *Snapshot) { s.Intent = intent })
}

// Start runs the poll loop until ctx is cancelled. It probes once
// immediately so /api/openclaw/status returns real data within a
// second of boot, then schedules the next probe adaptively: the
// configured interval (default 3s) while alive, IdlePollInterval (5s)
// while not. Start blocks; callers launch it in a goroutine.
func (m *Manager) Start(ctx context.Context) {
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
