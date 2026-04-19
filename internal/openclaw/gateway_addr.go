package openclaw

// gateway_addr.go resolves and caches the OpenClaw gateway's listen
// address so the fast HTTP liveness probe stays correct when the
// upstream port changes (operator edits openclaw.json, `openclaw
// gateway --port NNN`, dev profile, etc.). The authoritative source is
// `openclaw gateway status --json`, which merges the launchd/systemd
// service args with the user's config — it is reliable even when the
// service is stopped, and its output survives `openclaw gateway
// restart` because it re-reads the platform service definition.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultGatewayHost and DefaultGatewayPort are the historical bind
// pair documented in the OpenClaw README. We seed the address cache
// with them so the fast path is usable on the very first tick — the
// resolver then overwrites the seed as soon as the CLI answers.
const (
	DefaultGatewayHost = "127.0.0.1"
	DefaultGatewayPort = 18789
)

// AddressRefreshInterval caps how often the background resolver runs
// on its own. On-demand refreshes (triggered by a connection-refused
// probe) can happen more frequently but are serialised by addrResolve.
const AddressRefreshInterval = 5 * time.Minute

// GatewayAddress is the parsed result of one resolver run. Source
// mirrors the `portSource` field of `openclaw gateway status --json`
// ("service args" / "config" / …) plus our own sentinels ("default"
// for the pre-resolve seed, "cache" for recycled values).
type GatewayAddress struct {
	Host       string
	Port       int
	Source     string
	ResolvedAt time.Time
}

// HTTPHealthURL returns the URL the fast liveness probe should hit.
// Falls back to the historical default pair if either field is zero —
// no caller should see a zero value in production, but defending here
// keeps the probe path branch-free.
func (g GatewayAddress) HTTPHealthURL() string {
	h := g.Host
	if h == "" {
		h = DefaultGatewayHost
	}
	p := g.Port
	if p <= 0 {
		p = DefaultGatewayPort
	}
	return fmt.Sprintf("http://%s:%d/health", h, p)
}

// gatewayStatusEnvelope is the subset of `openclaw gateway status
// --json` we consume. Additional upstream fields are ignored by the
// JSON decoder.
type gatewayStatusEnvelope struct {
	Gateway struct {
		BindHost   string `json:"bindHost"`
		Port       int    `json:"port"`
		PortSource string `json:"portSource"`
	} `json:"gateway"`
}

// addrCache is an atomic pointer so probeFast can read it lock-free on
// every tick. addrResolve serialises concurrent resolvers — we do not
// want the startup resolver and an on-demand refresh (triggered by a
// connection-refused probe) spawning two Node processes at once.
var (
	addrCache   atomic.Pointer[GatewayAddress]
	addrResolve sync.Mutex
)

func init() {
	seed := &GatewayAddress{
		Host:   DefaultGatewayHost,
		Port:   DefaultGatewayPort,
		Source: "default",
	}
	addrCache.Store(seed)
}

// LoadGatewayAddress returns the currently cached address. Always
// non-nil thanks to the init-time seed.
func LoadGatewayAddress() GatewayAddress { return *addrCache.Load() }

// storeGatewayAddress replaces the cache pointer.
func storeGatewayAddress(a GatewayAddress) { addrCache.Store(&a) }

// ResolveGatewayAddress spawns `openclaw gateway status --json`, parses
// the response, and updates the cache. Returns the new address (not
// the cached copy) so callers can log the transition. On error the
// cache is left untouched — the old value keeps serving probes.
func ResolveGatewayAddress(ctx context.Context, runner Runner) (GatewayAddress, error) {
	addrResolve.Lock()
	defer addrResolve.Unlock()
	res, err := runner.runCmd(ctx, ProbeTimeout, "gateway", "status", "--json")
	if err != nil {
		return GatewayAddress{}, err
	}
	if res.ExitCode != 0 {
		return GatewayAddress{}, fmt.Errorf("gateway status exit %d: %s", res.ExitCode, trimErr(res.Stderr))
	}
	var env gatewayStatusEnvelope
	if err := json.Unmarshal(res.Stdout, &env); err != nil {
		return GatewayAddress{}, fmt.Errorf("parse gateway status: %w", err)
	}
	if env.Gateway.Port <= 0 {
		return GatewayAddress{}, fmt.Errorf("gateway status reported port=%d", env.Gateway.Port)
	}
	host := env.Gateway.BindHost
	if host == "" {
		host = DefaultGatewayHost
	}
	source := env.Gateway.PortSource
	if source == "" {
		source = "service args"
	}
	addr := GatewayAddress{
		Host:       host,
		Port:       env.Gateway.Port,
		Source:     source,
		ResolvedAt: time.Now().UTC(),
	}
	storeGatewayAddress(addr)
	return addr, nil
}
