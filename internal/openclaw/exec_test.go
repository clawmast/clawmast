package openclaw

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeFakeBin writes a small shell script that mimics openclaw CLI
// behavior for tests. The script dispatches on its first argument.
// Tests pass this via Runner.Binary to avoid coupling to the real
// openclaw install.
func writeFakeBin(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-bin tests require a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-openclaw")
	script := "#!/bin/sh\n" + body
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestRunCmdSuccess verifies a clean exit 0 returns the stdout bytes
// unchanged and Killed=false.
func TestRunCmdSuccess(t *testing.T) {
	bin := writeFakeBin(t, `echo '{"ok":true}'; exit 0`)
	r := Runner{Binary: bin}
	res, err := r.runCmd(context.Background(), 3*time.Second, "health", "--json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(string(res.Stdout)) != `{"ok":true}` {
		t.Fatalf("stdout: got %q", res.Stdout)
	}
	if res.Killed {
		t.Fatalf("killed=true on clean exit")
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit: want 0 got %d", res.ExitCode)
	}
}

// TestRunCmdNonZeroExit captures the exit code without returning an
// error, per the contract — callers decide what the code means.
func TestRunCmdNonZeroExit(t *testing.T) {
	bin := writeFakeBin(t, `echo boom 1>&2; exit 7`)
	r := Runner{Binary: bin}
	res, err := r.runCmd(context.Background(), 3*time.Second, "health")
	if err != nil {
		t.Fatalf("run returned error: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("exit: want 7 got %d", res.ExitCode)
	}
	if !strings.Contains(string(res.Stderr), "boom") {
		t.Fatalf("stderr: want boom, got %q", res.Stderr)
	}
}

// TestRunCmdTimeoutKills verifies the upstream-#11843 workaround:
// a process that refuses to exit is SIGKILLed and Killed=true is set.
func TestRunCmdTimeoutKills(t *testing.T) {
	bin := writeFakeBin(t, `echo partial; sleep 10; echo late`)
	r := Runner{Binary: bin}
	start := time.Now()
	res, err := r.runCmd(context.Background(), 150*time.Millisecond, "hang")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !res.Killed {
		t.Fatalf("killed=false on timeout")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout did not enforce (elapsed=%s)", elapsed)
	}
}

// TestRunCmdMissingBinary returns the ErrBinaryMissing sentinel when
// the default binary is not on PATH.
func TestRunCmdMissingBinary(t *testing.T) {
	r := Runner{} // DefaultBinary, assume not installed in clean env
	t.Setenv("PATH", t.TempDir())
	_, err := r.runCmd(context.Background(), time.Second, "health")
	if err != ErrBinaryMissing {
		t.Fatalf("want ErrBinaryMissing got %v", err)
	}
}
