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

// runBuild cross-compiles release binaries for every target in
// -platforms and tarballs each one to <out>/clawmast-<os>-<arch>.tar.gz.
//
// The tarball layout follows the Tailscale convention so install.sh
// and future package maintainers find exactly what they expect:
//
//	clawmast-<os>-<arch>/
//	  clawmast            worker binary (all tiers)
//	  clawmastd           supervisor binary (Tier 1 only, AGENTS.md R5)
//	  systemd/clawmastd.service            (Linux)
//	  launchd/com.clawmast.clawmastd.plist (macOS)
//
// The Windows tarball ships only clawmast.exe inside the prefix dir —
// no supervisor, no service units (R5: Windows is worker-only, manual
// install, supervisor parity deferred until after v1.0.0).
//
// selfupdate.Apply strips the top-level prefix and pulls out only the
// worker binary, so a single tarball serves both the auto-update flow
// (worker hot-swap) and the first-install flow (install.sh --source=
// release), matching how Tailscale, Syncthing, and Ollama distribute
// their daemon+client pairs.
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
	templates := fs.String("templates", "scripts/templates",
		"directory holding service unit templates (clawmastd.service.tmpl, "+
			"com.clawmast.clawmastd.plist.tmpl) bundled into Tier 1 tarballs")
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
		if err := buildOne(*out, *version, *commit, buildTime, osName, archName, *templates); err != nil {
			return fmt.Errorf("build %s/%s: %w", osName, archName, err)
		}
	}
	return nil
}

// tarEntry is one file destined for the release tarball, described
// once so the walk that emits headers stays uniform regardless of
// whether the source is a cross-compiled binary or a template plucked
// off disk.
type tarEntry struct {
	// name is the path inside the tarball, relative to the top-level
	// clawmast-<os>-<arch>/ prefix directory (which writeTarGz adds
	// automatically so callers here stay layout-agnostic).
	name string
	// srcPath is the absolute path of the source file on disk.
	srcPath string
	// mode is the file mode stored in the tar header. Binaries get
	// 0o755 so the executable bit survives round-tripping through
	// extractors that honour the header mode; text files get 0o644.
	mode int64
}

// buildOne cross-compiles a single os/arch, then wraps the artefacts
// in a gzip-tar archive following the Tailscale-style layout
// documented on runBuild. The temporary build directory is cleaned up
// even on failure so a re-run never picks up stale artefacts.
//
// Tier 1 (darwin/linux) tarballs carry both binaries plus the service
// unit template matching the host's init system. Tier 2 (windows)
// tarballs carry only clawmast.exe because AGENTS.md R5 defers the
// Windows supervisor until after v1.0.0.
func buildOne(outDir, version, commit, buildTime, osName, archName, templatesDir string) error {
	tmpDir, err := os.MkdirTemp("", "clawmast-build-")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Windows is worker-only per R5; skipping clawmastd here keeps the
	// Windows tarball free of a binary that has no service unit to
	// supervise and no auto-update path to hot-swap it.
	buildSupervisor := osName != "windows"

	workerExt := ""
	if osName == "windows" {
		workerExt = ".exe"
	}
	workerPath := filepath.Join(tmpDir, "clawmast"+workerExt)
	if err := goBuild(workerPath, "./cmd/clawmast", osName, archName, version, commit, buildTime); err != nil {
		return fmt.Errorf("build worker: %w", err)
	}
	entries := []tarEntry{
		{name: "clawmast" + workerExt, srcPath: workerPath, mode: 0o755},
	}

	if buildSupervisor {
		supervisorPath := filepath.Join(tmpDir, "clawmastd")
		if err := goBuild(supervisorPath, "./cmd/clawmastd", osName, archName, version, commit, buildTime); err != nil {
			return fmt.Errorf("build supervisor: %w", err)
		}
		entries = append(entries, tarEntry{
			name: "clawmastd", srcPath: supervisorPath, mode: 0o755,
		})

		// Bundle the init-system unit template so install.sh and
		// downstream packagers do not need a repo checkout to render
		// it. The .tmpl suffix is preserved in the tarball because
		// the files still carry @@TOKEN@@ placeholders that depend on
		// the user's CLAWMAST_HOME; naming them "clawmastd.service"
		// flat would invite packagers to drop them into systemd
		// verbatim and watch the service fail with "No such file or
		// directory" on startup.
		switch osName {
		case "linux":
			entries = append(entries, tarEntry{
				name:    "systemd/clawmastd.service.tmpl",
				srcPath: filepath.Join(templatesDir, "clawmastd.service.tmpl"),
				mode:    0o644,
			})
		case "darwin":
			entries = append(entries, tarEntry{
				name:    "launchd/com.clawmast.clawmastd.plist.tmpl",
				srcPath: filepath.Join(templatesDir, "com.clawmast.clawmastd.plist.tmpl"),
				mode:    0o644,
			})
		}
	}

	tarballName := fmt.Sprintf("clawmast-%s-%s.tar.gz", osName, archName)
	tarballPath := filepath.Join(outDir, tarballName)
	prefix := fmt.Sprintf("clawmast-%s-%s", osName, archName)
	if err := writeTarGz(tarballPath, prefix, entries); err != nil {
		return fmt.Errorf("tarball %s: %w", tarballPath, err)
	}
	fmt.Fprintf(os.Stderr, "[release] wrote %s\n", tarballPath)
	return nil
}

// goBuild invokes `go build` for one cmd package, cross-compiled to
// osName/archName. Split out of buildOne so the same flags (trimpath,
// -s -w, version ldflags, CGO disabled) are used identically for the
// worker and the supervisor — a skew between the two would show up as
// confusing version-string mismatches in the field.
func goBuild(outPath, pkg, osName, archName, version, commit, buildTime string) error {
	ldflags := fmt.Sprintf("-s -w "+
		"-X github.com/clawmast/clawmast/internal/version.Version=%s "+
		"-X github.com/clawmast/clawmast/internal/version.Commit=%s "+
		"-X github.com/clawmast/clawmast/internal/version.BuildTime=%s",
		version, commit, buildTime)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags,
		"-o", outPath, pkg)
	cmd.Env = append(os.Environ(),
		"GOOS="+osName,
		"GOARCH="+archName,
		"CGO_ENABLED=0",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	fmt.Fprintf(os.Stderr, "[release] building %s/%s %s -> %s\n", osName, archName, pkg, outPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build %s: %w", pkg, err)
	}
	return nil
}

// writeTarGz emits a gzip-tar archive whose every entry is nested under
// a single top-level prefix directory. The prefix matches the tarball's
// basename (e.g. clawmast-linux-amd64/) so an operator running `tar xzf
// clawmast-linux-amd64.tar.gz` ends up with a self-labelling directory
// in cwd — the same convention Tailscale, Syncthing, and Ollama use.
//
// selfupdate.extractTar strips the prefix on the auto-update path so
// versions/<ver>/ keeps the historical flat layout, and install.sh
// references files by their nested names when consuming the tarball
// as an installation source.
func writeTarGz(outPath, prefix string, entries []tarEntry) error {
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	// Emit the prefix directory first so extractors that rely on an
	// explicit TypeDir header (BSD tar, some minimal busybox builds)
	// create the wrapper before any regular files land inside it.
	if err := tw.WriteHeader(&tar.Header{
		Name:     prefix + "/",
		Mode:     0o755,
		Typeflag: tar.TypeDir,
		ModTime:  time.Now().UTC(),
	}); err != nil {
		return err
	}

	// Track directories we have already emitted so a bundle containing
	// multiple files under systemd/ or launchd/ (future service
	// variants, companion units) does not trip extractors that refuse
	// duplicate TypeDir headers.
	seenDirs := map[string]bool{"": true}
	for _, e := range entries {
		relDir := filepath.ToSlash(filepath.Dir(e.name))
		if relDir == "." {
			relDir = ""
		}
		if relDir != "" && !seenDirs[relDir] {
			if err := tw.WriteHeader(&tar.Header{
				Name:     prefix + "/" + relDir + "/",
				Mode:     0o755,
				Typeflag: tar.TypeDir,
				ModTime:  time.Now().UTC(),
			}); err != nil {
				return err
			}
			seenDirs[relDir] = true
		}

		info, err := os.Stat(e.srcPath)
		if err != nil {
			return fmt.Errorf("stat %s: %w", e.srcPath, err)
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     prefix + "/" + filepath.ToSlash(e.name),
			Mode:     e.mode,
			Size:     info.Size(),
			Typeflag: tar.TypeReg,
			ModTime:  info.ModTime(),
		}); err != nil {
			return err
		}
		body, err := os.Open(e.srcPath)
		if err != nil {
			return fmt.Errorf("open %s: %w", e.srcPath, err)
		}
		if _, err := io.Copy(tw, body); err != nil {
			_ = body.Close()
			return fmt.Errorf("copy %s: %w", e.srcPath, err)
		}
		if err := body.Close(); err != nil {
			return fmt.Errorf("close %s: %w", e.srcPath, err)
		}
	}
	return nil
}
