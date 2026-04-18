package main

import (
	"archive/tar"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// defaultPlatforms is the build matrix every `clawmast-release build`
// emits unless the caller overrides it with -platforms. Tier 1 is
// always present; Tier 2 (windows/amd64, AGENTS.md R5 — worker-only,
// no supervisor) is included so the release carries a manual-install
// Windows artefact alongside the supervised macOS/Linux ones.
var defaultPlatforms = []string{
	"darwin/amd64",
	"darwin/arm64",
	"linux/amd64",
	"linux/arm64",
	"windows/amd64",
}

// runBuild cross-compiles the worker binary for every target in
// -platforms and tarballs each one to <out>/clawmast-<os>-<arch>.tar.gz.
// The tarball contains a single top-level entry named "clawmast" (or
// "clawmast.exe" on Windows), matching what selfupdate.Apply expects
// to find when it extracts a downloaded artefact.
//
// This command is the authoritative source of release artefacts: the
// GitHub Actions release workflow invokes it with the tag version so
// the resulting tarballs are referenced directly by manifest.json.
func runBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	out := fs.String("out", "dist", "output directory for tarballs")
	version := fs.String("version", "", "semver version label (required, e.g. v0.2.0)")
	commit := fs.String("commit", "", "git sha injected into internal/version.Commit (defaults to version)")
	platforms := fs.String("platforms", "",
		"comma-separated list of os/arch targets (default: the built-in matrix)")
	_ = fs.Parse(args)
	if *version == "" {
		return fmt.Errorf("-version is required")
	}
	if *commit == "" {
		*commit = *version
	}
	targets := defaultPlatforms
	if *platforms != "" {
		targets = strings.Split(*platforms, ",")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", *out, err)
	}
	buildTime := time.Now().UTC().Format(time.RFC3339)
	for _, target := range targets {
		osName, archName, ok := strings.Cut(target, "/")
		if !ok {
			return fmt.Errorf("invalid platform %q: want os/arch", target)
		}
		if err := buildOne(*out, *version, *commit, buildTime, osName, archName); err != nil {
			return fmt.Errorf("build %s/%s: %w", osName, archName, err)
		}
	}
	return nil
}

// buildOne cross-compiles a single os/arch, then wraps the binary in
// a gzip-tar archive. The temporary build directory is cleaned up
// even on failure so a re-run never picks up stale artefacts.
func buildOne(outDir, version, commit, buildTime, osName, archName string) error {
	tmpDir, err := os.MkdirTemp("", "clawmast-build-")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	binName := "clawmast"
	if osName == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmpDir, binName)
	ldflags := fmt.Sprintf("-s -w "+
		"-X github.com/clawmast/clawmast/internal/version.Version=%s "+
		"-X github.com/clawmast/clawmast/internal/version.Commit=%s "+
		"-X github.com/clawmast/clawmast/internal/version.BuildTime=%s",
		version, commit, buildTime)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags,
		"-o", binPath, "./cmd/clawmast")
	cmd.Env = append(os.Environ(),
		"GOOS="+osName,
		"GOARCH="+archName,
		"CGO_ENABLED=0",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	fmt.Fprintf(os.Stderr, "[release] building %s/%s -> %s\n", osName, archName, binPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build: %w", err)
	}

	tarballName := fmt.Sprintf("clawmast-%s-%s.tar.gz", osName, archName)
	tarballPath := filepath.Join(outDir, tarballName)
	if err := writeTarGz(tarballPath, binPath, binName); err != nil {
		return fmt.Errorf("tarball %s: %w", tarballPath, err)
	}
	fmt.Fprintf(os.Stderr, "[release] wrote %s\n", tarballPath)
	return nil
}

// writeTarGz writes a single-file gzip-tar archive to outPath. The
// tar entry carries the binary's real mode so extractors on unix
// preserve the executable bit; the tarball layout matches what
// selfupdate.extractTarGz already knows how to consume.
func writeTarGz(outPath, srcPath, entryName string) error {
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	info, err := os.Stat(srcPath)
	if err != nil {
		return err
	}
	hdr := &tar.Header{
		Name:    entryName,
		Mode:    0o755,
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	body, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer body.Close()
	if _, err := io.Copy(tw, body); err != nil {
		return err
	}
	return nil
}
