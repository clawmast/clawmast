// `clawmast logs` — tail the supervisor + worker log files that
// launchd / systemd append to under <install_root>/logs/. The worker
// logs via slog to stderr which the plist redirects into
// clawmastd.err.log, so a single file actually carries both streams;
// we still print both paths so a future layout change doesn't silently
// drop one stream from the output.
//
// Flags:
//
//	-n N   tail the last N lines per file (default 200)
//	-f     follow mode: after the initial tail, print new bytes as
//	       they arrive. Ctrl-C to exit.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func runLogs(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	n := fs.Int("n", 200, "lines to print per file")
	follow := fs.Bool("f", false, "follow the file (like tail -f)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	root := resolveCLIInstallRoot()
	if root == "" {
		fmt.Fprintln(stderr, "clawmast: could not determine install root")
		return 1
	}
	logDir := filepath.Join(root, "logs")
	files := []string{
		filepath.Join(logDir, "clawmastd.out.log"),
		filepath.Join(logDir, "clawmastd.err.log"),
	}

	found := false
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			continue
		}
		found = true
		fmt.Fprintf(stdout, "==> %s <==\n", f)
		if err := printTail(stdout, f, *n); err != nil {
			fmt.Fprintf(stderr, "clawmast: tail %s: %v\n", f, err)
		}
	}
	if !found {
		fmt.Fprintf(stderr, "clawmast: no log files under %s\n", logDir)
		fmt.Fprintln(stderr, "  hint: this install may not be running under launchd / systemd")
		fmt.Fprintln(stderr, "        on linux, try: journalctl --user -u clawmastd -f")
		return 1
	}
	if !*follow {
		return 0
	}
	// Follow mode: poll the err log for new bytes. Kept intentionally
	// simple — no fsnotify dep — because launchd appends in-place and
	// our tail reader re-opens on truncation below. The out log is
	// usually silent (plist only writes stderr there on edge cases) so
	// a single follower covers the 95% case.
	return followFile(stdout, stderr, filepath.Join(logDir, "clawmastd.err.log"))
}

// printTail streams the last n lines of path to w. It tolerates files
// smaller than n lines and never reads more than 4 MiB into memory.
func printTail(w io.Writer, path string, n int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	const maxRead = 4 << 20
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	start := int64(0)
	if fi.Size() > maxRead {
		start = fi.Size() - maxRead
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return err
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, ln := range lines {
		fmt.Fprintln(w, ln)
	}
	return nil
}

// followFile implements tail -f against a single path. It handles
// truncation (launchd rotates by renaming, not truncating, but systemd
// with logrotate does) by re-opening when the inode changes.
func followFile(stdout, stderr io.Writer, path string) int {
	var (
		f      *os.File
		reader *bufio.Reader
		inode  uint64
	)
	reopen := func() error {
		if f != nil {
			_ = f.Close()
		}
		nf, err := os.Open(path)
		if err != nil {
			return err
		}
		fi, err := nf.Stat()
		if err != nil {
			_ = nf.Close()
			return err
		}
		_, _ = nf.Seek(0, io.SeekEnd)
		f = nf
		reader = bufio.NewReader(nf)
		inode = inodeOf(fi)
		return nil
	}
	if err := reopen(); err != nil {
		fmt.Fprintf(stderr, "clawmast: open %s: %v\n", path, err)
		return 1
	}
	defer f.Close()
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			_, _ = stdout.Write([]byte(line))
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, io.EOF) {
			fmt.Fprintf(stderr, "clawmast: read %s: %v\n", path, err)
			return 1
		}
		time.Sleep(250 * time.Millisecond)
		if fi, err := os.Stat(path); err == nil && inodeOf(fi) != inode {
			_ = reopen()
		}
	}
}
