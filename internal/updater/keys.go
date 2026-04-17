package updater

import (
	_ "embed"
	"fmt"

	"aead.dev/minisign"
)

// devPubKey is the development minisign public key embedded into every
// binary. It is used by default when CLAWMAST_UPDATE_PUBKEY is not set.
//
// Key ID 6D3113C6E0D40175. Rotating this key — Decision #6 in
// architecture/refactor.md §10 — will land together with the first
// signed production release; until then the embedded key is
// sufficient for the auto-update flow to be "enforced" rather than
// optional.
//
//go:embed devkey.pub
var devPubKeyText []byte

// DevPublicKey returns the embedded development minisign public key.
// A parse error here means the build is corrupt; it is reported as a
// fatal-shaped error so callers in main() can fail fast.
func DevPublicKey() (minisign.PublicKey, error) {
	var pk minisign.PublicKey
	if err := pk.UnmarshalText(devPubKeyText); err != nil {
		return minisign.PublicKey{}, fmt.Errorf("updater: embedded dev pubkey is unparseable: %w", err)
	}
	return pk, nil
}

// MustDevPublicKey is the panicking form of DevPublicKey for use in
// startup paths where the embedded key's validity is a build-time
// invariant. The corresponding test exercises this code path.
func MustDevPublicKey() minisign.PublicKey {
	pk, err := DevPublicKey()
	if err != nil {
		panic(err)
	}
	return pk
}
