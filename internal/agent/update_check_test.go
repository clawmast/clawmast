package agent_test

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func fakeChannel(t *testing.T, manifest, sig []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/"+updater.ManifestFilename, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(manifest)
	})
	mux.HandleFunc("/"+updater.SignatureFilename, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(sig)
	})
	return httptest.NewServer(mux)
}
