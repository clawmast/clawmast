package openclaw

import (
	"context"
	"testing"
	"time"
)

// withSeedAddress swaps the package-level address cache for the
// duration of a test and restores whatever was there before. Every
// test that mutates the cache MUST call this so parallel tests cannot
// see each other's state.
func withSeedAddress(t *testing.T, a GatewayAddress) {
	t.Helper()
	prev := LoadGatewayAddress()
	storeGatewayAddress(a)
	t.Cleanup(func() { storeGatewayAddress(prev) })
}

// TestGatewayAddressHTTPHealthURL pins the URL shape the fast probe
// depends on and asserts the defaults kick in when either field is
// zero — belt and braces for a hot-path helper.
func TestGatewayAddressHTTPHealthURL(t *testing.T) {
	cases := []struct {
		name string
		in   GatewayAddress
		want string
	}{
		{"full", GatewayAddress{Host: "10.0.0.4", Port: 22100}, "http://10.0.0.4:22100/health"},
		{"empty host", GatewayAddress{Port: 18789}, "http://127.0.0.1:18789/health"},
		{"zero port", GatewayAddress{Host: "127.0.0.1"}, "http://127.0.0.1:18789/health"},
		{"zero value", GatewayAddress{}, "http://127.0.0.1:18789/health"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.in.HTTPHealthURL(); got != c.want {
				t.Fatalf("HTTPHealthURL=%q want %q", got, c.want)
			}
		})
	}
}

// TestResolveGatewayAddressHappy feeds the resolver a fake CLI that
// emits the canonical `gateway status --json` shape and verifies the
// cache picks up a non-default port.
func TestResolveGatewayAddressHappy(t *testing.T) {
	withSeedAddress(t, GatewayAddress{Host: "127.0.0.1", Port: DefaultGatewayPort, Source: "default"})
	bin := writeFakeBin(t, `cat <<'EOF'
{"gateway":{"bindHost":"127.0.0.1","port":22100,"portSource":"config"}}
EOF`)
	addr, err := ResolveGatewayAddress(context.Background(), Runner{Binary: bin})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if addr.Port != 22100 {
		t.Fatalf("port=%d want 22100", addr.Port)
	}
	if addr.Source != "config" {
		t.Fatalf("source=%q want config", addr.Source)
	}
	if time.Since(addr.ResolvedAt) > time.Minute {
		t.Fatalf("ResolvedAt not stamped")
	}
	if got := LoadGatewayAddress().Port; got != 22100 {
		t.Fatalf("cache port=%d want 22100", got)
	}
	if got := LoadGatewayAddress().HTTPHealthURL(); got != "http://127.0.0.1:22100/health" {
		t.Fatalf("HTTPHealthURL=%q", got)
	}
}

// TestResolveGatewayAddressRejectsZeroPort makes sure a malformed CLI
// reply does not silently poison the cache with port=0.
func TestResolveGatewayAddressRejectsZeroPort(t *testing.T) {
	seed := GatewayAddress{Host: "127.0.0.1", Port: 18789, Source: "seed"}
	withSeedAddress(t, seed)
	bin := writeFakeBin(t, `echo '{"gateway":{"bindHost":"127.0.0.1","port":0}}'`)
	_, err := ResolveGatewayAddress(context.Background(), Runner{Binary: bin})
	if err == nil {
		t.Fatalf("expected error on port=0")
	}
	if got := LoadGatewayAddress().Port; got != 18789 {
		t.Fatalf("cache mutated; port=%d want 18789", got)
	}
}

// TestResolveGatewayAddressLeavesCacheOnExitNonZero confirms that a
// CLI failure keeps the previous address serving probes — critical
// for rolling upgrades where `gateway status` may flap.
func TestResolveGatewayAddressLeavesCacheOnExitNonZero(t *testing.T) {
	seed := GatewayAddress{Host: "127.0.0.1", Port: 19999, Source: "seed"}
	withSeedAddress(t, seed)
	bin := writeFakeBin(t, `echo "boom" 1>&2; exit 1`)
	_, err := ResolveGatewayAddress(context.Background(), Runner{Binary: bin})
	if err == nil {
		t.Fatalf("expected error on exit 1")
	}
	if got := LoadGatewayAddress().Port; got != 19999 {
		t.Fatalf("cache port=%d want 19999", got)
	}
}

// TestResolveGatewayAddressMissingBindHostDefaults ensures that a
// status payload omitting bindHost falls back to the loopback default
// rather than building "http://:18789/health".
func TestResolveGatewayAddressMissingBindHostDefaults(t *testing.T) {
	withSeedAddress(t, GatewayAddress{})
	bin := writeFakeBin(t, `echo '{"gateway":{"port":21000,"portSource":"service args"}}'`)
	addr, err := ResolveGatewayAddress(context.Background(), Runner{Binary: bin})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if addr.Host != DefaultGatewayHost {
		t.Fatalf("host=%q want %q", addr.Host, DefaultGatewayHost)
	}
	if got := addr.HTTPHealthURL(); got != "http://127.0.0.1:21000/health" {
		t.Fatalf("url=%q", got)
	}
}
