package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateChannel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		wantOK bool
		want   string
	}{
		{"stable", true, "stable"},
		{"STABLE", true, "stable"},
		{"beta", true, "beta"},
		{" beta\n", true, "beta"},
		{"", false, ""},
		{"nightly", false, ""},
	}
	for _, c := range cases {
		got, err := ValidateChannel(c.in)
		if c.wantOK {
			if err != nil {
				t.Errorf("ValidateChannel(%q) err = %v; want nil", c.in, err)
				continue
			}
			if got != c.want {
				t.Errorf("ValidateChannel(%q) = %q; want %q", c.in, got, c.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("ValidateChannel(%q) err = nil; want error", c.in)
			continue
		}
		if !errors.Is(err, ErrUnknownChannel) {
			t.Errorf("ValidateChannel(%q) err = %v; want ErrUnknownChannel", c.in, err)
		}
	}
}

func TestChannelPreferenceRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Missing file reads as "no preference".
	got, err := ReadChannelPreference(dir)
	if err != nil {
		t.Fatalf("ReadChannelPreference before write: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty preference, got %q", got)
	}

	// Write -> read round-trip.
	written, err := WriteChannelPreference(dir, "beta")
	if err != nil {
		t.Fatalf("WriteChannelPreference: %v", err)
	}
	if written != "beta" {
		t.Fatalf("WriteChannelPreference returned %q", written)
	}
	got, err = ReadChannelPreference(dir)
	if err != nil {
		t.Fatalf("ReadChannelPreference after write: %v", err)
	}
	if got != "beta" {
		t.Fatalf("read back %q; want beta", got)
	}

	// Corrupt file silently degrades to "" (callers fall back to env
	// / default) rather than failing boot.
	if err := os.WriteFile(filepath.Join(dir, ChannelFileName), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = ReadChannelPreference(dir)
	if err != nil {
		t.Fatalf("ReadChannelPreference corrupt: %v", err)
	}
	if got != "" {
		t.Fatalf("corrupt preference returned %q; want empty", got)
	}
}

func TestWriteChannelPreferenceRejectsUnknown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := WriteChannelPreference(dir, "nightly")
	if !errors.Is(err, ErrUnknownChannel) {
		t.Fatalf("want ErrUnknownChannel; got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ChannelFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("file should not exist after rejected write; stat = %v", statErr)
	}
}

func TestResolveChannelOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Default when nothing set.
	v, src := ResolveChannel(dir, "", "")
	if v != ChannelStable || src != "default" {
		t.Fatalf("default: got (%q, %q); want (stable, default)", v, src)
	}

	// Env wins over default when no file exists.
	v, src = ResolveChannel(dir, "beta", "")
	if v != "beta" || src != "env" {
		t.Fatalf("env: got (%q, %q); want (beta, env)", v, src)
	}

	// File wins over env.
	if _, err := WriteChannelPreference(dir, "stable"); err != nil {
		t.Fatal(err)
	}
	v, src = ResolveChannel(dir, "beta", "")
	if v != "stable" || src != "preference-file" {
		t.Fatalf("file: got (%q, %q); want (stable, preference-file)", v, src)
	}

	// Bad env falls through to default.
	if err := os.Remove(filepath.Join(dir, ChannelFileName)); err != nil {
		t.Fatal(err)
	}
	v, src = ResolveChannel(dir, "nightly", "")
	if v != ChannelStable || src != "default" {
		t.Fatalf("bad env: got (%q, %q); want (stable, default)", v, src)
	}
}
