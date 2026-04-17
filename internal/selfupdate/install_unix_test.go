//go:build unix

package selfupdate_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/clawmast/clawmast/internal/selfupdate"
	"github.com/clawmast/clawmast/internal/updater"
)

// buildTarball returns a gzip'd tar archive with one regular file
// "clawmast" containing binary.
func buildTarball(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{
			Name:    name,
			Mode:    0o755,
			Size:    int64(len(content)),
			ModTime: time.Now(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("tw.Write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tw.Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz.Close: %v", err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// makeManifest returns a manifest whose sole artifact points at url
// with the given body's sha/size.
func makeManifest(url string, body []byte) *updater.Manifest {
	return &updater.Manifest{
		Channel:     "stable",
		Version:     "v9.9.9",
		PublishedAt: time.Now().UTC(),
		Artifacts: []updater.Artifact{{
			OS:     runtime.GOOS,
			Arch:   runtime.GOARCH,
			URL:    url,
			Size:   int64(len(body)),
			SHA256: sha256Hex(body),
		}},
	}
}

func TestApplyHappyPathFirstInstall(t *testing.T) {
	body := buildTarball(t, map[string][]byte{
		"clawmast":      []byte("fake worker binary\n"),
		"MANIFEST.json": []byte(`{"version":"v9.9.9"}`),
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	root := t.TempDir()
	res, err := selfupdate.Apply(context.Background(), selfupdate.Config{
		Manifest:    makeManifest(srv.URL+"/artifact.tar.gz", body),
		InstallRoot: root,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Version != "v9.9.9" || res.CurrentAfter != "versions/v9.9.9" {
		t.Errorf("unexpected result: %+v", res)
	}
	if res.CurrentBefore != "" || res.PreviousAfter != "" {
		t.Errorf("expected empty before/previous on first install, got %+v", res)
	}

	// clawmast binary extracted with +x.
	info, err := os.Stat(filepath.Join(root, "versions", "v9.9.9", "clawmast"))
	if err != nil {
		t.Fatalf("stat clawmast: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("clawmast missing +x: mode=%o", info.Mode().Perm())
	}
	// current symlink points at versions/v9.9.9.
	cur, err := os.Readlink(filepath.Join(root, "current"))
	if err != nil || cur != "versions/v9.9.9" {
		t.Fatalf("current symlink: %q err=%v", cur, err)
	}
}

func TestApplyRotatesPreviousOnUpgrade(t *testing.T) {
	body := buildTarball(t, map[string][]byte{
		"clawmast": []byte("v2 body"),
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	root := t.TempDir()
	// Pre-seed an existing install that Apply should rotate into previous.
	if err := os.MkdirAll(filepath.Join(root, "versions", "v1.0.0"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink("versions/v1.0.0", filepath.Join(root, "current")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	m := makeManifest(srv.URL+"/artifact.tar.gz", body)
	m.Version = "v2.0.0"
	res, err := selfupdate.Apply(context.Background(), selfupdate.Config{
		Manifest:    m,
		InstallRoot: root,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.CurrentBefore != "versions/v1.0.0" || res.PreviousAfter != "versions/v1.0.0" {
		t.Errorf("unexpected rotation result: %+v", res)
	}
	if res.CurrentAfter != "versions/v2.0.0" {
		t.Errorf("unexpected current after: %q", res.CurrentAfter)
	}
}

func TestApplyRejectsShaMismatch(t *testing.T) {
	body := buildTarball(t, map[string][]byte{"clawmast": []byte("content")})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	m := makeManifest(srv.URL+"/artifact.tar.gz", body)
	m.Artifacts[0].SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	_, err := selfupdate.Apply(context.Background(), selfupdate.Config{
		Manifest:    m,
		InstallRoot: t.TempDir(),
	})
	if !errors.Is(err, selfupdate.ErrBadSHA256) {
		t.Fatalf("want ErrBadSHA256, got %v", err)
	}
}

func TestApplyRejectsOversizedBody(t *testing.T) {
	body := buildTarball(t, map[string][]byte{"clawmast": []byte("some content here")})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	m := makeManifest(srv.URL+"/artifact.tar.gz", body)
	m.Artifacts[0].Size = int64(len(body) - 5) // claim smaller than reality
	_, err := selfupdate.Apply(context.Background(), selfupdate.Config{
		Manifest:    m,
		InstallRoot: t.TempDir(),
	})
	if !errors.Is(err, selfupdate.ErrSizeMismatch) {
		t.Fatalf("want ErrSizeMismatch, got %v", err)
	}
}

func TestApplyRejectsMissingArtifactForHost(t *testing.T) {
	m := &updater.Manifest{
		Channel:     "stable",
		Version:     "v9.9.9",
		PublishedAt: time.Now().UTC(),
		Artifacts: []updater.Artifact{{
			OS: "plan9", Arch: "mips",
			URL:    "https://example.com/x.tar.gz",
			Size:   1,
			SHA256: sha256Hex([]byte{0}),
		}},
	}
	_, err := selfupdate.Apply(context.Background(), selfupdate.Config{
		Manifest:    m,
		InstallRoot: t.TempDir(),
	})
	if !errors.Is(err, selfupdate.ErrNoArtifactForHost) {
		t.Fatalf("want ErrNoArtifactForHost, got %v", err)
	}
}

func TestApplyShortCircuitsSameVersion(t *testing.T) {
	body := buildTarball(t, map[string][]byte{"clawmast": []byte("same")})
	m := makeManifest("http://127.0.0.1/unused", body)
	_, err := selfupdate.Apply(context.Background(), selfupdate.Config{
		Manifest:       m,
		InstallRoot:    t.TempDir(),
		CurrentVersion: m.Version,
	})
	if !errors.Is(err, selfupdate.ErrAlreadyInstalled) {
		t.Fatalf("want ErrAlreadyInstalled, got %v", err)
	}
}

// Guard against an accidental change to the error wrapping chain.
func TestErrorsExported(t *testing.T) {
	for _, e := range []error{
		selfupdate.ErrBadSHA256,
		selfupdate.ErrSizeMismatch,
		selfupdate.ErrNoArtifactForHost,
		selfupdate.ErrAlreadyInstalled,
		selfupdate.ErrBadTarball,
	} {
		if e == nil {
			t.Fatalf("expected non-nil sentinel")
		}
		if !errors.Is(fmt.Errorf("wrap: %w", e), e) {
			t.Fatalf("%v not detectable via errors.Is when wrapped", e)
		}
	}
}
