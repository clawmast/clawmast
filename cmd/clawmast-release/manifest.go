package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/clawmast/clawmast/internal/updater"
)

// runManifest walks a directory of tarballs produced by `build` and
// writes an updater.Manifest to -out. Each artefact URL is joined
// from -base-url + the tarball filename, so the canonical layout is:
//
//	<base-url>/clawmast-<os>-<arch>.tar.gz
//
// For GitHub Releases the caller passes
// -base-url=https://github.com/<org>/<repo>/releases/download/<tag>,
// which matches the URL softprops/action-gh-release assigns to
// release assets.
//
// The resulting manifest is subsequently signed with `sign`; neither
// step mutates the tarballs, so the manifest's sha256 values stay
// consistent with whatever actually ships.
func runManifest(args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	dir := fs.String("dir", "dist", "directory holding clawmast-<os>-<arch>.tar.gz tarballs")
	out := fs.String("out", "", "path to write manifest.json (required)")
	channel := fs.String("channel", "stable", "release channel name")
	version := fs.String("version", "", "semver version (required, e.g. v0.2.0)")
	baseURL := fs.String("base-url", "",
		"absolute URL prefix the artefacts will be served from (required)")
	notes := fs.String("notes", "", "optional release notes blurb")
	platforms := fs.String("platforms", "",
		"comma-separated os/arch targets to include (default: everything in -dir)")
	_ = fs.Parse(args)
	if *out == "" || *version == "" || *baseURL == "" {
		return fmt.Errorf("-out, -version and -base-url are required")
	}
	if !strings.HasPrefix(*version, "v") {
		return fmt.Errorf("-version must start with 'v' (got %q)", *version)
	}
	trimmedBase := strings.TrimRight(*baseURL, "/")

	targets, err := resolveTargets(*dir, *platforms)
	if err != nil {
		return err
	}
	m := updater.Manifest{
		Channel:     *channel,
		Version:     *version,
		PublishedAt: time.Now().UTC(),
		Notes:       *notes,
	}
	for _, t := range targets {
		tarballName := fmt.Sprintf("clawmast-%s-%s.tar.gz", t.osName, t.archName)
		tarballPath := filepath.Join(*dir, tarballName)
		size, sum, err := sizeAndSHA256(tarballPath)
		if err != nil {
			return fmt.Errorf("hash %s: %w", tarballPath, err)
		}
		m.Artifacts = append(m.Artifacts, updater.Artifact{
			OS:     t.osName,
			Arch:   t.archName,
			URL:    trimmedBase + "/" + tarballName,
			Size:   size,
			SHA256: sum,
		})
	}
	if err := m.Validate(); err != nil {
		return fmt.Errorf("validate manifest: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	fmt.Fprintf(os.Stderr, "[release] wrote %s (%d artefacts)\n", *out, len(m.Artifacts))
	return nil
}

// target pairs a GOOS/GOARCH for manifest assembly.
type target struct{ osName, archName string }

// resolveTargets returns the platform list, either parsed from the
// caller's -platforms flag or discovered from the directory contents
// (every clawmast-<os>-<arch>.tar.gz becomes one entry).
func resolveTargets(dir, platforms string) ([]target, error) {
	if platforms != "" {
		var out []target
		for _, p := range strings.Split(platforms, ",") {
			osName, archName, ok := strings.Cut(p, "/")
			if !ok {
				return nil, fmt.Errorf("invalid platform %q: want os/arch", p)
			}
			out = append(out, target{osName, archName})
		}
		return out, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []target
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "clawmast-") || !strings.HasSuffix(name, ".tar.gz") {
			continue
		}
		stem := strings.TrimSuffix(strings.TrimPrefix(name, "clawmast-"), ".tar.gz")
		osName, archName, ok := strings.Cut(stem, "-")
		if !ok {
			continue
		}
		out = append(out, target{osName, archName})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no clawmast-*.tar.gz tarballs found in %s", dir)
	}
	return out, nil
}

// sizeAndSHA256 returns the byte count and lowercase hex sha256 of
// the file at path, in one pass.
func sizeAndSHA256(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}
