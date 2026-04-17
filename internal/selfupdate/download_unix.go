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

// extractTar walks a tar reader and materialises entries under dir.
// Three safety rules apply:
//
//  1. Paths are cleaned and must not escape dir (no "../foo").
//  2. Only regular files, directories, and symlinks are accepted —
//     device nodes, FIFOs, and hardlinks return ErrBadTarball.
//  3. Symlink targets are rejected if they resolve outside dir.
//
// Mode bits are preserved for regular files so the worker binary lands
// with its +x bit set; directories are forced to 0o700 to match the
// perms install.sh uses for versions/.
func extractTar(tr *tar.Reader, dir string) error {
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: tar header: %v", ErrBadTarball, err)
		}
		rel := filepath.Clean(hdr.Name)
		if rel == "." || rel == "" {
			continue
		}
		if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			return fmt.Errorf("%w: unsafe path %q", ErrBadTarball, hdr.Name)
		}
		out := filepath.Join(dir, rel)
		// Defence in depth: the final path must still be inside dir
		// even after symlink traversal in an earlier entry.
		if rel, err := filepath.Rel(dir, out); err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("%w: escaped root %q", ErrBadTarball, hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(out, 0o700); err != nil {
				return fmt.Errorf("selfupdate: mkdir %s: %w", out, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
				return fmt.Errorf("selfupdate: mkdir parent: %w", err)
			}
			f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
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
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) {
				return fmt.Errorf("%w: absolute symlink %q -> %q", ErrBadTarball, hdr.Name, hdr.Linkname)
			}
			if err := os.Symlink(hdr.Linkname, out); err != nil {
				return fmt.Errorf("selfupdate: symlink %s: %w", out, err)
			}
		default:
			return fmt.Errorf("%w: unsupported entry type %c in %q", ErrBadTarball, hdr.Typeflag, hdr.Name)
		}
	}
}
