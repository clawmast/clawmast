package updater_test

import (
	"testing"

	"github.com/clawmast/clawmast/internal/updater"
)

func TestDevPublicKeyParses(t *testing.T) {
	pk, err := updater.DevPublicKey()
	if err != nil {
		t.Fatalf("DevPublicKey: %v", err)
	}
	// The key ID is a hint, not a secret. Matching it here guards
	// against an accidental rotation where the committed .pub file
	// gets swapped without noticing. This is the production signing
	// key rotated in at v0.1.0-rc.1; see internal/updater/keys.go
	// for why we treat it as permanent.
	// Cast through uint64 because the high bit is set in the prod
	// key ID; an untyped hex constant > 1<<63 can't bind to the int
	// formal that t.Fatalf's variadic expects for %X via reflection.
	const wantID uint64 = 0xFD412C2F6CA2B447
	if got := pk.ID(); got != wantID {
		t.Fatalf("embedded key ID: want %016X got %016X", wantID, got)
	}
}

func TestMustDevPublicKeyDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v", r)
		}
	}()
	_ = updater.MustDevPublicKey()
}
