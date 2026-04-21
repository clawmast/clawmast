package updater

import (
	_ "embed"
	"fmt"

	"aead.dev/minisign"
)

// devPubKeyText is the production minisign public key embedded into
// every binary. It is used by default when CLAWMAST_UPDATE_PUBKEY is
// not set, which is the normal production path. The identifier and
// the filename are kept as "dev*" for git-history continuity with
// iteration-2 commits; the key material itself was rotated to prod
// at v0.1.0-rc.1 and is no longer a development key.
//
// Key ID FD412C2F6CA2B447. The private half lives only in the
// maintainer's password manager and in the release workflow's
// MINISIGN_PRIVATE_KEY secret. Rotating it — Decision #6 in
// architecture/refactor.md §10 — requires a client-side rotation
// protocol that does not yet exist, so the key is treated as
// permanent for every shipped binary.
//
//go:embed devkey.pub
var devPubKeyText []byte

// DevPublicKey returns the embedded minisign public key. The "Dev"
// prefix is historical (see devPubKeyText); the returned key is the
// production channel key since v0.1.0-rc.1. A parse error here means
// the build is corrupt, which is reported as a fatal-shaped error so
// callers in main() can fail fast.
func DevPublicKey() (minisign.PublicKey, error) {
	var pk minisign.PublicKey
	if err := pk.UnmarshalText(devPubKeyText); err != nil {
		return minisign.PublicKey{}, fmt.Errorf("updater: embedded pubkey is unparseable: %w", err)
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
