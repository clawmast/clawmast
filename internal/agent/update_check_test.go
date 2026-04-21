package agent_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aead.dev/minisign"

	"github.com/clawmast/clawmast/internal/agent"
	"github.com/clawmast/clawmast/internal/updater"
)

// TestUpdateCheckNotConfigured verifies the zero-config code path:
// with UpdateBaseURL empty the handler must report
// source="not-configured" rather than pretending to have talked to a
// channel.
func TestUpdateCheckNotConfigured(t *testing.T) {
	resp := callUpdateCheck(t, agent.Config{})
	if resp.Source != "not-configured" {
		t.Fatalf("source: want not-configured got %q (resp=%+v)", resp.Source, resp)
	}
	if resp.UpdateAvailable {
		t.Fatalf("update_available must be false when unconfigured")
	}
	if resp.Current == "" || resp.Current != resp.Latest {
		t.Fatalf("current/latest should equal the running version; got %+v", resp)
	}
}

// TestUpdateCheckSignedManifest covers the happy path: a fake channel
// serves a manifest signed with a test keypair, the handler verifies
// against the injected pubkey, and returns source="signed-manifest"
// with real version/channel data.
func TestUpdateCheckSignedManifest(t *testing.T) {
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	m := updater.Manifest{
		Channel: "stable", Version: "v9.9.9",
		PublishedAt: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC),
		Notes:       "hello",
		Artifacts: []updater.Artifact{{
			OS: "darwin", Arch: "arm64",
			URL: "https://example.com/a.tgz", Size: 1,
			SHA256: strings.Repeat("a", 64),
		}},
	}
	body, _ := json.Marshal(m)
	sig := minisign.Sign(priv, body)

	srv := fakeChannel(t, body, sig)
	defer srv.Close()

	resp := callUpdateCheck(t, agent.Config{
		UpdateBaseURL: srv.URL,
		UpdateChannel: "stable",
		UpdatePubKey:  pub,
	})
	if resp.Source != "signed-manifest" {
		t.Fatalf("source: want signed-manifest got %q (note=%q err=%q)", resp.Source, resp.Note, resp.ErrorCode)
	}
	if resp.Latest != "v9.9.9" {
		t.Fatalf("latest: got %q", resp.Latest)
	}
	if !resp.UpdateAvailable {
		t.Fatalf("update_available should be true when latest != current")
	}
	if resp.Notes != "hello" {
		t.Fatalf("notes: got %q", resp.Notes)
	}
	if resp.PublishedAt == "" {
		t.Fatalf("published_at should be populated")
	}
}

// TestUpdateCheckBadSignature ensures the handler surfaces
// verification failure with a machine-readable error_code so the UI
// can render a hard warning.
func TestUpdateCheckBadSignature(t *testing.T) {
	_, priv, _ := minisign.GenerateKey(rand.Reader)
	pub2, _, _ := minisign.GenerateKey(rand.Reader)

	body, _ := json.Marshal(updater.Manifest{
		Channel: "stable", Version: "v9.9.9",
		PublishedAt: time.Now().UTC(),
		Artifacts: []updater.Artifact{{
			OS: "darwin", Arch: "arm64",
			URL: "https://example.com/a.tgz", Size: 1,
			SHA256: strings.Repeat("a", 64),
		}},
	})
	sig := minisign.Sign(priv, body) // signed by priv, but handler holds pub2

	srv := fakeChannel(t, body, sig)
	defer srv.Close()

	resp := callUpdateCheck(t, agent.Config{
		UpdateBaseURL: srv.URL,
		UpdatePubKey:  pub2,
	})
	if resp.Source != "error" || resp.ErrorCode != "bad-signature" {
		t.Fatalf("want source=error error_code=bad-signature, got %+v", resp)
	}
	if resp.UpdateAvailable {
		t.Fatalf("update_available must be false on signature failure")
	}
}

func callUpdateCheck(t *testing.T, cfg agent.Config) agent.UpdateCheckResponse {
	t.Helper()
	s := agent.NewServer(cfg)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	httpResp, err := http.Post(ts.URL+"/api/updates/check", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer httpResp.Body.Close()
	raw, _ := io.ReadAll(httpResp.Body)
	var resp agent.UpdateCheckResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return resp
}

// fakeChannel serves manifest + signature under the "/stable/" prefix
// to match updater.Client's URL composition, which now appends the
// channel name between BaseURL and the manifest filename. Tests that
// need a different channel prefix should inline their own server.
func fakeChannel(t *testing.T, manifest, sig []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/stable/"+updater.ManifestFilename, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(manifest)
	})
	mux.HandleFunc("/stable/"+updater.SignatureFilename, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(sig)
	})
	return httptest.NewServer(mux)
}

// fakeChannelCounted is the same fake with a hit counter, used by the
// background-loop test to block until the goroutine has actually done
// work rather than racing against a bare sleep.
func fakeChannelCounted(t *testing.T, manifest, sig []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/stable/"+updater.ManifestFilename, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(manifest)
	})
	mux.HandleFunc("/stable/"+updater.SignatureFilename, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(sig)
	})
	return httptest.NewServer(mux), &hits
}

// TestRunBackgroundChecksPollsAndCaches boots the loop with a 10ms
// interval pointed at a counted fake channel, waits for at least two
// manifest fetches, and then asserts the last check landed in the
// cache. The cancellation path exits the loop deterministically.
func TestRunBackgroundChecksPollsAndCaches(t *testing.T) {
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	body, _ := json.Marshal(updater.Manifest{
		Channel: "stable", Version: "v9.9.9",
		PublishedAt: time.Now().UTC(),
		Artifacts: []updater.Artifact{{
			OS: "darwin", Arch: "arm64",
			URL: "https://example.com/a.tgz", Size: 1,
			SHA256: strings.Repeat("a", 64),
		}},
	})
	sig := minisign.Sign(priv, body)

	srv, hits := fakeChannelCounted(t, body, sig)
	defer srv.Close()

	s := agent.NewServer(agent.Config{
		UpdateBaseURL:   srv.URL,
		UpdateChannel:   "stable",
		UpdatePubKey:    pub,
		CheckInterval:   10 * time.Millisecond,
		FirstCheckDelay: time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.RunBackgroundChecks(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := hits.Load(); got < 2 {
		cancel()
		<-done
		t.Fatalf("expected >=2 manifest fetches within 2s, got %d", got)
	}

	snap, at := s.LastBackgroundCheck()
	if snap == nil {
		cancel()
		<-done
		t.Fatalf("cache should be populated after ticks")
	}
	if snap.Source != "signed-manifest" || snap.Latest != "v9.9.9" {
		cancel()
		<-done
		t.Fatalf("cached snap wrong: %+v", snap)
	}
	if at.IsZero() {
		t.Fatalf("cache timestamp must be set")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("RunBackgroundChecks did not exit after cancel")
	}
}

// TestRunBackgroundChecksDisabled asserts that a zero CheckInterval
// is a hard no-op: the call returns immediately without touching the
// cache or panicking. This is the "standalone, no supervisor" path
// where we don't want an orphan goroutine if the caller forgets to
// cancel.
func TestRunBackgroundChecksDisabled(t *testing.T) {
	s := agent.NewServer(agent.Config{
		UpdateBaseURL: "http://127.0.0.1:1", // would fail fast if called
		// CheckInterval left at 0 — the whole point.
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.RunBackgroundChecks(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("RunBackgroundChecks with zero interval must return immediately")
	}
	if snap, _ := s.LastBackgroundCheck(); snap != nil {
		t.Fatalf("cache must stay empty when loop is disabled, got %+v", snap)
	}
}

// TestRunBackgroundChecksSurvivesErrors feeds the loop a channel that
// returns tampered manifests so every tick hits bad-signature. The
// cache must land on Source=error with the classified code, and the
// loop must not wedge — a second error tick has to land too.
func TestRunBackgroundChecksSurvivesErrors(t *testing.T) {
	_, priv, _ := minisign.GenerateKey(rand.Reader)
	otherPub, _, _ := minisign.GenerateKey(rand.Reader)

	body, _ := json.Marshal(updater.Manifest{
		Channel: "stable", Version: "v9.9.9",
		PublishedAt: time.Now().UTC(),
		Artifacts: []updater.Artifact{{
			OS: "darwin", Arch: "arm64",
			URL: "https://example.com/a.tgz", Size: 1,
			SHA256: strings.Repeat("a", 64),
		}},
	})
	sig := minisign.Sign(priv, body) // signed by priv — handler holds otherPub

	srv, hits := fakeChannelCounted(t, body, sig)
	defer srv.Close()

	s := agent.NewServer(agent.Config{
		UpdateBaseURL:   srv.URL,
		UpdateChannel:   "stable",
		UpdatePubKey:    otherPub,
		CheckInterval:   10 * time.Millisecond,
		FirstCheckDelay: time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.RunBackgroundChecks(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := hits.Load(); got < 2 {
		t.Fatalf("expected loop to keep polling through errors, got %d fetches", got)
	}
	snap, _ := s.LastBackgroundCheck()
	if snap == nil || snap.Source != "error" || snap.ErrorCode != "bad-signature" {
		t.Fatalf("expected cached error=bad-signature, got %+v", snap)
	}
}
