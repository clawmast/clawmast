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
	// gets swapped without noticing.
	const wantID = 0x6D3113C6E0D40175
	if got := pk.ID(); got != wantID {
		t.Fatalf("embedded dev key ID: want %016X got %016X", wantID, got)
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
