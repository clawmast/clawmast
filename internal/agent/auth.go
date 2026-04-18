package agent

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// TokenLen is the raw byte length of the generated bearer token. 32
// bytes encodes to 64 hex chars — short enough to paste, long enough
// to make brute-force pointless.
const TokenLen = 32

// Token represents the clawmast worker's HTTP bearer token. The zero
// value is "no auth configured" and is treated as a hard error for
// non-loopback clients — there is no "off" mode in I3.
type Token struct {
	// Value is the 64-char hex secret. Never logged in full; the
	// server logs a short fingerprint on boot and refuses to log
	// further occurrences.
	Value string
	// Path is the on-disk location the token was loaded from or
	// persisted to. Empty when loaded from env only.
	Path string
	// Generated is true when this Token was freshly created on this
	// boot. The server prints a one-time copy-paste hint in that case.
	Generated bool
}

// LoadOrCreateToken returns the bearer token for this worker. It
// prefers in this order:
//
//  1. CLAWMAST_BEARER_TOKEN env var (useful for tests / k8s-style
//     deployments where the operator injects secrets).
//  2. Contents of <stateDir>/token.
//  3. A freshly generated 32-byte token, persisted to <stateDir>/token
//     with 0600 permissions.
//
// stateDir is typically ~/.clawmast/state. An empty stateDir skips
// step 2 and forces step 3 into memory-only (returning an empty Path).
// Callers that want persistence must pass a writable directory.
func LoadOrCreateToken(stateDir string) (Token, error) {
	if v := strings.TrimSpace(os.Getenv("CLAWMAST_BEARER_TOKEN")); v != "" {
		return Token{Value: v}, nil
	}
	var path string
	if stateDir != "" {
		path = filepath.Join(stateDir, "token")
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			s := strings.TrimSpace(string(raw))
			if s != "" {
				return Token{Value: s, Path: path}, nil
			}
		case errors.Is(err, fs.ErrNotExist):
			// fall through to generation
		default:
			return Token{}, fmt.Errorf("auth: read %s: %w", path, err)
		}
	}
	buf := make([]byte, TokenLen)
	if _, err := rand.Read(buf); err != nil {
		return Token{}, fmt.Errorf("auth: generate token: %w", err)
	}
	val := hex.EncodeToString(buf)
	tok := Token{Value: val, Path: path, Generated: true}
	if path != "" {
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return Token{}, fmt.Errorf("auth: mkdir %s: %w", stateDir, err)
		}
		if err := os.WriteFile(path, []byte(val+"\n"), 0o600); err != nil {
			return Token{}, fmt.Errorf("auth: write %s: %w", path, err)
		}
	}
	return tok, nil
}

// Fingerprint is the first 8 hex chars of the token, safe to log.
func (t Token) Fingerprint() string {
	if len(t.Value) < 8 {
		return ""
	}
	return t.Value[:8]
}

// bearerMiddleware wraps next with Authorization: Bearer checking.
// Loopback requests are exempt per I3 contract (refactor.md §9:
// "bearer-token auth on the HTTP port so remote access over LAN /
// public IP is not anonymous"); public paths in authPublic are also
// exempt so launchd / uptime monitors can probe /api/health without
// credentials.
func bearerMiddleware(tok Token, authPublic map[string]bool, log *slog.Logger, next http.Handler) http.Handler {
	want := []byte(tok.Value)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authPublic[r.URL.Path] || isLoopback(r) {
			next.ServeHTTP(w, r)
			return
		}
		// Non-/api/* routes (the embedded UI) are allowed without
		// bearer so the first-load HTML can run; the UI's JS then
		// presents a token modal before hitting /api/*.
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		h := r.Header.Get("Authorization")
		got, ok := strings.CutPrefix(h, "Bearer ")
		if !ok {
			unauthorized(w, "missing bearer token")
			return
		}
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			log.Warn("bearer token mismatch",
				"component", "agent",
				"path", r.URL.Path,
				"remote", r.RemoteAddr)
			unauthorized(w, "bearer token rejected")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopback returns true when the request came in on a loopback
// address. Used to keep the local dashboard zero-friction.
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

func unauthorized(w http.ResponseWriter, reason string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="clawmast"`)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized","reason":"` + reason + `"}` + "\n"))
}
