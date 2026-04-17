package updater_test

import (
	"strings"
	"testing"
	"time"

	"github.com/clawmast/clawmast/internal/updater"
)

func TestManifestForHost(t *testing.T) {
	m := updater.Manifest{
		Artifacts: []updater.Artifact{
			{OS: "darwin", Arch: "arm64"},
			{OS: "linux", Arch: "amd64"},
		},
	}
	if a, ok := m.ForHost("linux", "amd64"); !ok || a.OS != "linux" {
		t.Fatalf("linux/amd64: got %+v ok=%v", a, ok)
	}
	if _, ok := m.ForHost("windows", "amd64"); ok {
		t.Fatalf("windows/amd64 should miss; got hit")
	}
}

func TestManifestValidate(t *testing.T) {
	good := updater.Manifest{
		Channel: "stable", Version: "v0.2.0",
		PublishedAt: time.Now().UTC(),
		Artifacts: []updater.Artifact{{
			OS: "darwin", Arch: "arm64",
			URL: "https://example.com/a.tgz", Size: 1,
			SHA256: strings.Repeat("a", 64),
		}},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good: %v", err)
	}

	cases := []struct {
		name  string
		mutate func(*updater.Manifest)
		want   string
	}{
		{"empty channel", func(m *updater.Manifest) { m.Channel = "" }, "channel is empty"},
		{"bad version", func(m *updater.Manifest) { m.Version = "0.2.0" }, "missing the leading"},
		{"zero time", func(m *updater.Manifest) { m.PublishedAt = time.Time{} }, "published_at is zero"},
		{"no artifacts", func(m *updater.Manifest) { m.Artifacts = nil }, "no artifacts"},
		{"http url", func(m *updater.Manifest) { m.Artifacts[0].URL = "http://evil.example.com/a.tgz" }, "must be https"},
		{"zero size", func(m *updater.Manifest) { m.Artifacts[0].Size = 0 }, "size must be > 0"},
		{"short sha", func(m *updater.Manifest) { m.Artifacts[0].SHA256 = "abc" }, "sha256 must be 64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := good
			m.Artifacts = append([]updater.Artifact(nil), good.Artifacts...)
			tc.mutate(&m)
			err := m.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}
