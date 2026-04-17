package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aead.dev/minisign"
)

// Default* constants capture the Iteration 2 contract. They are not
// meant to be changed at runtime — callers override BaseURL on the
// Client for development or channel switching.
const (
	// DefaultChannel is the channel name used when the caller does
	// not specify one. The CI release pipeline writes manifests for
	// both "stable" and "beta"; T2-01 only plumbs "stable".
	DefaultChannel = "stable"

	// ManifestFilename / SignatureFilename name the two files every
	// channel URL must serve.
	ManifestFilename  = "manifest.json"
	SignatureFilename = "manifest.json.minisig"

	// maxManifestBytes bounds the manifest download to keep a
	// compromised or misbehaving channel host from exhausting memory.
	// A signed manifest listing a few dozen artifacts should be well
	// under 64 KiB in practice.
	maxManifestBytes = 256 * 1024

	// defaultHTTPTimeout is intentionally tight — the UI blocks on
	// the "check for updates" round-trip.
	defaultHTTPTimeout = 10 * time.Second
)

// Sentinel errors exposed so callers can branch on verification
// failure vs transport failure without string matching.
var (
	ErrBadSignature    = errors.New("updater: manifest signature verification failed")
	ErrManifestMissing = errors.New("updater: manifest not found at channel URL")
	ErrBadURL          = errors.New("updater: base URL is invalid")
	ErrManifestTooBig  = errors.New("updater: manifest exceeds size limit")
	ErrChannelMismatch = errors.New("updater: manifest channel does not match requested channel")
)

// Client fetches and verifies manifests from one channel. Zero value
// is not usable; construct via New.
type Client struct {
	// BaseURL is the directory that serves manifest.json and
	// manifest.json.minisig — for example
	// "https://update.clawmast.com/stable" or
	// "https://github.com/clawmast/clawmast/releases/download/channel-stable".
	// The trailing slash is optional.
	BaseURL string

	// Channel is the channel name the caller expects the manifest
	// to advertise. Defaults to DefaultChannel.
	Channel string

	// PubKey is the minisign public key the manifest signature must
	// verify against.
	PubKey minisign.PublicKey

	// HTTP is the client used for manifest + signature fetches. If
	// nil, a client with defaultHTTPTimeout is used.
	HTTP *http.Client
}

// New returns a ready-to-use Client pointed at baseURL using key for
// signature verification. An empty channel defaults to DefaultChannel.
func New(baseURL, channel string, key minisign.PublicKey) *Client {
	if channel == "" {
		channel = DefaultChannel
	}
	return &Client{
		BaseURL: baseURL,
		Channel: channel,
		PubKey:  key,
		HTTP:    &http.Client{Timeout: defaultHTTPTimeout},
	}
}

// Check fetches the manifest and its signature, verifies the
// signature against c.PubKey, parses the JSON, and returns a validated
// Manifest. It does not touch disk.
func (c *Client) Check(ctx context.Context) (*Manifest, error) {
	manifestURL, sigURL, err := c.urls()
	if err != nil {
		return nil, err
	}
	manifestBytes, err := c.fetch(ctx, manifestURL, true)
	if err != nil {
		return nil, err
	}
	sigBytes, err := c.fetch(ctx, sigURL, false)
	if err != nil {
		return nil, err
	}
	if !minisign.Verify(c.PubKey, manifestBytes, sigBytes) {
		return nil, fmt.Errorf("%w (channel=%s)", ErrBadSignature, c.Channel)
	}
	var m Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return nil, fmt.Errorf("updater: manifest is signed but not valid JSON: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if m.Channel != c.Channel {
		return nil, fmt.Errorf("%w: want %q got %q", ErrChannelMismatch, c.Channel, m.Channel)
	}
	return &m, nil
}

func (c *Client) urls() (string, string, error) {
	if c.BaseURL == "" {
		return "", "", ErrBadURL
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", "", fmt.Errorf("%w: %q", ErrBadURL, c.BaseURL)
	}
	base := strings.TrimRight(c.BaseURL, "/")
	return base + "/" + ManifestFilename, base + "/" + SignatureFilename, nil
}

func (c *Client) fetch(ctx context.Context, url string, allowMissing bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "clawmast-updater/1")
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("updater: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && allowMissing {
		return nil, fmt.Errorf("%w: %s", ErrManifestMissing, url)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("updater: fetch %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("updater: read %s: %w", url, err)
	}
	if int64(len(body)) > maxManifestBytes {
		return nil, fmt.Errorf("%w: %s", ErrManifestTooBig, url)
	}
	return body, nil
}
