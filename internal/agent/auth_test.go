package agent_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawmast/clawmast/internal/agent"
)

// TestLoadOrCreateTokenFromFile verifies that a pre-existing token on
// disk is loaded verbatim and marked non-generated.
func TestLoadOrCreateTokenFromFile(t *testing.T) {
	dir := t.TempDir()
	os.Unsetenv("CLAWMAST_BEARER_TOKEN")
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("deadbeef\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tok, err := agent.LoadOrCreateToken(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	if tok.Value != "deadbeef" {
		t.Fatalf("value: want deadbeef got %q", tok.Value)
	}
	if tok.Generated {
		t.Fatalf("generated=true for seeded token")
	}
}

// TestLoadOrCreateTokenGenerate verifies the first-boot path: no file,
// token is fresh, persisted with 0600.
func TestLoadOrCreateTokenGenerate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")
	os.Unsetenv("CLAWMAST_BEARER_TOKEN")
	tok, err := agent.LoadOrCreateToken(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	if !tok.Generated {
		t.Fatalf("generated=false for first-boot")
	}
	if len(tok.Value) != 2*agent.TokenLen {
		t.Fatalf("value len: want %d got %d", 2*agent.TokenLen, len(tok.Value))
	}
	st, err := os.Stat(filepath.Join(dir, "token"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm: want 0600 got %o", perm)
	}
}

// TestLoadOrCreateTokenEnvWins verifies the env override takes priority
// over on-disk state.
func TestLoadOrCreateTokenEnvWins(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("onfile"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("CLAWMAST_BEARER_TOKEN", "fromenv")
	tok, err := agent.LoadOrCreateToken(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	if tok.Value != "fromenv" {
		t.Fatalf("value: want fromenv got %q", tok.Value)
	}
}

// TestBearerRejectsNonLoopbackWithoutToken verifies a non-loopback
// request without a valid bearer token is rejected with 401.
func TestBearerRejectsNonLoopbackWithoutToken(t *testing.T) {
	ts := newTestServerWithToken(t, "shhh")
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/version", nil)
	req.RemoteAddr = "203.0.113.10:44444"
	// httptest.Server populates RemoteAddr server-side from the
	// actual TCP connection, so we exercise the middleware directly
	// via mustCallWithRemote below instead.
	body, status := mustCallWithRemote(t, ts, "/api/version", "", "203.0.113.10:44444")
	if status != http.StatusUnauthorized {
		t.Fatalf("status: want 401 got %d body=%s", status, body)
	}
	if !strings.Contains(body, "unauthorized") {
		t.Fatalf("body: want 'unauthorized', got %s", body)
	}
}

// TestBearerAcceptsValidToken verifies a correct Authorization header
// lets the request through, even from a non-loopback RemoteAddr.
func TestBearerAcceptsValidToken(t *testing.T) {
	ts := newTestServerWithToken(t, "shhh")
	defer ts.Close()
	body, status := mustCallWithRemote(t, ts, "/api/version", "shhh", "203.0.113.10:44444")
	if status != http.StatusOK {
		t.Fatalf("status: want 200 got %d body=%s", status, body)
	}
}

// TestBearerAllowsHealthPublic verifies /api/health stays public for
// launchd / uptime probes even when a token is configured and the
// request comes from outside loopback.
func TestBearerAllowsHealthPublic(t *testing.T) {
	ts := newTestServerWithToken(t, "shhh")
	defer ts.Close()
	_, status := mustCallWithRemote(t, ts, "/api/health", "", "203.0.113.10:44444")
	if status != http.StatusOK {
		t.Fatalf("/api/health: want 200 got %d", status)
	}
}

// TestBearerAllowsLoopback verifies loopback clients skip the bearer
// check so the local dashboard is zero-config.
func TestBearerAllowsLoopback(t *testing.T) {
	ts := newTestServerWithToken(t, "shhh")
	defer ts.Close()
	// httptest.Server is bound on 127.0.0.1, so a plain client
	// request arrives with an IsLoopback RemoteAddr.
	resp, err := http.Get(ts.URL + "/api/version")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback: want 200 got %d", resp.StatusCode)
	}
}

func newTestServerWithToken(t *testing.T, token string) *httptest.Server {
	t.Helper()
	s := agent.NewServer(agent.Config{Token: agent.Token{Value: token}})
	return httptest.NewServer(s.Handler())
}

// mustCallWithRemote simulates a non-loopback request by overriding
// the Host portion of the target URL. httptest.Server binds to
// 127.0.0.1, so we reach the middleware via its net.SplitHostPort path
// using a custom Dialer that rewrites the address to the real
// loopback one but keeps the Host header pointing at a public-looking
// RemoteAddr via net.Pipe + http.Transport hooks. Simpler path:
// invoke the handler directly with a rewritten RemoteAddr.
func mustCallWithRemote(t *testing.T, ts *httptest.Server, path, token, remote string) (string, int) {
	t.Helper()
	req := httptest.NewRequest("GET", ts.URL+path, nil)
	req.RemoteAddr = remote
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
