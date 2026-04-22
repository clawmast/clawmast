//go:build unix

package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/clawmast/clawmast/internal/updater"
)

// downloadAndExtract streams the artifact URL, hashes while reading
// so we never hold the full body in memory, then tees into a tar.gz
// extractor rooted at dir. Returns the number of bytes actually read.
//
// Both defences run in a single streaming pass:
//
//   - size: we read at most Artifact.Size+1 bytes; a trailing byte means
//     the server is serving more than the manifest claims and we abort.
//   - sha256: computed over the bytes we read, compared to the manifest
//     hex string at the end. Any mismatch is ErrBadSHA256, which callers
//     surface as HTTP 502 — the manifest is signed, a hash mismatch means
//     the artifact was tampered with between signer and worker.
func downloadAndExtract(ctx context.Context, client *http.Client, art updater.Artifact, dir string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, art.URL, nil)
	if err != nil {
		return 0, fmt.Errorf("selfupdate: build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("selfupdate: download %s: %w", art.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("selfupdate: download %s: unexpected status %d", art.URL, resp.StatusCode)
	}

	// Cap read at Size+1: if we manage to read one extra byte, the
	// payload is larger than advertised and the manifest's sha/size
	// pair is no longer a safe integrity statement.
	hasher := sha256.New()
	capped := &io.LimitedReader{R: resp.Body, N: art.Size + 1}
	tee := io.TeeReader(capped, hasher)

	gz, err := gzip.NewReader(tee)
	if err != nil {
		return 0, fmt.Errorf("%w: gzip: %v", ErrBadTarball, err)
	}
	defer gz.Close()
	if err := extractTar(tar.NewReader(gz), dir); err != nil {
		return 0, err
	}

	// Drain any remaining bytes so the hash covers the full payload.
	// gzip typically stops reading at its trailer, leaving a few
	// bytes behind on well-formed archives that we still need to
	// hash. tee reads upstream of capped, so these bytes *are*
	// counted toward the Size+1 budget.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return 0, fmt.Errorf("selfupdate: drain body: %w", err)
	}
	readTotal := (art.Size + 1) - capped.N
	if capped.N == 0 {
		// Budget exhausted: the server advertised more than Size bytes.
		return 0, fmt.Errorf("%w: server returned more than Size=%d bytes", ErrSizeMismatch, art.Size)
	}
	if readTotal != art.Size {
		return 0, fmt.Errorf("%w: got %d, manifest says %d", ErrSizeMismatch, readTotal, art.Size)
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(got, art.SHA256) {
		return 0, fmt.Errorf("%w: got %s, manifest says %s", ErrBadSHA256, got, art.SHA256)
	}
	return readTotal, nil
}

// workerBinaryName is the tarball entry the auto-update path cares
// about. Everything else in the archive (clawmastd, service unit
// templates, release metadata files) is relevant only to the
// install.sh first-install flow and is ignored here so versions/<ver>/
// stays worker-only — matching the flat layout install.sh produces.
const workerBinaryName = "clawmast"

// extractTar walks a tar reader and writes the worker binary out at
// <dir>/clawmast, stripping an optional single top-level directory
// prefix. Two archive layouts are accepted:
//
//  1. Flat (legacy, pre-Tailscale-refactor): the worker lives at
//     tar root as "clawmast".
//  2. Prefixed (current): the worker lives at "clawmast-<os>-<arch>/
//     clawmast" alongside the supervisor and service templates that
//     only install.sh needs.
//
// Everything except the worker entry is silently dropped. That keeps
// the auto-update install tree free of supervisor binaries and
// templates that would otherwise require the versions/<ver>/
// directory layout — which install.sh and the supervisor assume is
// worker-only — to grow new conventions.
//
// Safety rules retained from the pre-refactor implementation:
//
//  1. Paths are cleaned and must not contain ".." or be absolute.
//  2. Only regular files are materialised; dir / symlink / device
//     entries are ignored (the worker never ships any of those).
//  3. The worker entry must be found before EOF or the tarball is
//     rejected with ErrBadTarball.
func extractTar(tr *tar.Reader, dir string) error {
	found := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: tar header: %v", ErrBadTarball, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		rel := filepath.ToSlash(filepath.Clean(hdr.Name))
		if rel == "." || rel == "" {
			continue
		}
		if strings.HasPrefix(rel, "../") || rel == ".." || strings.Contains(rel, "/../") || filepath.IsAbs(rel) {
			return fmt.Errorf("%w: unsafe path %q", ErrBadTarball, hdr.Name)
		}
		// Only the worker binary is forwarded. Split the cleaned path
		// and accept depth 0 ("clawmast") or depth 1
		// ("<prefix>/clawmast"); anything deeper is layout drift we
		// refuse to silently honour.
		parts := strings.Split(rel, "/")
		last := parts[len(parts)-1]
		if last != workerBinaryName {
			continue
		}
		if len(parts) > 2 {
			continue
		}

		if found {
			return fmt.Errorf("%w: duplicate worker entry %q", ErrBadTarball, hdr.Name)
		}
		out := filepath.Join(dir, workerBinaryName)
		// Defence in depth: the final path must still be inside dir
		// after cleaning. extractTar never creates intermediate dirs
		// anymore (we only write a single file at dir root) so
		// symlink-race style attacks have no foothold here.
		if relOut, relErr := filepath.Rel(dir, out); relErr != nil || strings.HasPrefix(relOut, "..") {
			return fmt.Errorf("%w: escaped root %q", ErrBadTarball, hdr.Name)
		}
		mode := os.FileMode(hdr.Mode) & 0o777
		if mode == 0 {
			// Some archivers omit mode bits; fall back to 0o755 so the
			// executable bit survives on readers that trust the header
			// mode verbatim.
			mode = 0o755
		}
		f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return fmt.Errorf("selfupdate: open %s: %w", out, err)
		}
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return fmt.Errorf("selfupdate: write %s: %w", out, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("selfupdate: close %s: %w", out, err)
		}
		found = true
	}
	if !found {
		return fmt.Errorf("%w: no %q entry in archive", ErrBadTarball, workerBinaryName)
	}
	return nil
}
