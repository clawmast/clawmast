package updater_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aead.dev/minisign"

	"github.com/clawmast/clawmast/internal/updater"
)

func TestClientCheckHappyPath(t *testing.T) {
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	manifest := updater.Manifest{
		Channel:     "stable",
		Version:     "v0.2.0",
		PublishedAt: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC),
		Notes:       "Iteration 2",
		Artifacts: []updater.Artifact{
			{
				OS: "darwin", Arch: "arm64",
				URL:    "https://github.com/clawmast/clawmast/releases/download/v0.2.0/clawmast-darwin-arm64.tar.gz",
				Size:   12345678,
				SHA256: strings.Repeat("a", 64),
			},
		},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sig := minisign.Sign(priv, body)

	srv := fakeChannel(t, "stable", body, sig)
	defer srv.Close()

	client := updater.New(srv.URL, "stable", pub)
	got, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.Version != "v0.2.0" {
		t.Fatalf("version: got %q", got.Version)
	}
	if got.Channel != "stable" {
		t.Fatalf("channel: got %q", got.Channel)
	}
	if a, ok := got.ForHost("darwin", "arm64"); !ok || a.Size != 12345678 {
		t.Fatalf("ForHost: got %+v ok=%v", a, ok)
	}
}

func TestClientCheckRejectsTamperedManifest(t *testing.T) {
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	good := mustMarshalManifest(t, "stable", "v0.2.0")
	sig := minisign.Sign(priv, good)

	tampered := strings.Replace(string(good), "v0.2.0", "v9.9.9", 1)
	srv := fakeChannel(t, "stable", []byte(tampered), sig)
	defer srv.Close()

	_, err = updater.New(srv.URL, "stable", pub).Check(context.Background())
	if !errors.Is(err, updater.ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestClientCheckRejectsChannelMismatch(t *testing.T) {
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	body := mustMarshalManifest(t, "beta", "v0.2.0")
	sig := minisign.Sign(priv, body)

	// Client is configured for "stable" so it requests
	// /stable/manifest.json; the fake server serves a beta-labelled
	// manifest there to exercise the channel-mismatch path.
	srv := fakeChannel(t, "stable", body, sig)
	defer srv.Close()

	_, err = updater.New(srv.URL, "stable", pub).Check(context.Background())
	if !errors.Is(err, updater.ErrChannelMismatch) {
		t.Fatalf("want ErrChannelMismatch, got %v", err)
	}
}

func TestClientCheckRejectsWrongKey(t *testing.T) {
	// A manifest signed by priv1 must not verify against pub2.
	_, priv1, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key1: %v", err)
	}
	pub2, _, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key2: %v", err)
	}
	body := mustMarshalManifest(t, "stable", "v0.2.0")
	sig := minisign.Sign(priv1, body)

	srv := fakeChannel(t, "stable", body, sig)
	defer srv.Close()

	_, err = updater.New(srv.URL, "stable", pub2).Check(context.Background())
	if !errors.Is(err, updater.ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestClientCheckReports404(t *testing.T) {
	pub, _, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err = updater.New(srv.URL, "stable", pub).Check(context.Background())
	if !errors.Is(err, updater.ErrManifestMissing) {
		t.Fatalf("want ErrManifestMissing, got %v", err)
	}
}

func TestClientCheckRejectsInvalidBaseURL(t *testing.T) {
	pub, _, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	for _, bad := range []string{"", "file:///tmp/x", "not a url"} {
		_, err := updater.New(bad, "stable", pub).Check(context.Background())
		if !errors.Is(err, updater.ErrBadURL) {
			t.Fatalf("base=%q: want ErrBadURL, got %v", bad, err)
		}
	}
}

// fakeChannel stands in for the HTTP channel host. channel is the
// per-channel URL prefix the client will actually GET (the client
// appends "/<channel>/manifest.json" to its BaseURL), so tests pass
// the same channel name they give to updater.New.
func fakeChannel(t *testing.T, channel string, manifest, sig []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/"+channel+"/"+updater.ManifestFilename, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(manifest)
	})
	mux.HandleFunc("/"+channel+"/"+updater.SignatureFilename, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(sig)
	})
	return httptest.NewServer(mux)
}

func mustMarshalManifest(t *testing.T, channel, version string) []byte {
	t.Helper()
	m := updater.Manifest{
		Channel:     channel,
		Version:     version,
		PublishedAt: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC),
		Artifacts: []updater.Artifact{{
			OS: "darwin", Arch: "arm64",
			URL:    "https://example.com/clawmast-darwin-arm64.tar.gz",
			Size:   1024,
			SHA256: strings.Repeat("b", 64),
		}},
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}
