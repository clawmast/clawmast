// Package updater fetches release manifests from a configured channel
// (GitHub Releases or, in production, https://update.clawmast.com) and
// verifies their minisign signature before any caller acts on the
// advertised version.
//
// The public surface is:
//
//   - Manifest / Artifact: JSON shape produced by the release pipeline.
//   - Client: fetches manifest.json + manifest.json.minisig, enforces
//     the signature, and returns a parsed Manifest.
//   - ErrBadSignature, ErrManifestMissing: sentinel errors callers can
//     distinguish without string-matching.
//
// The package is deliberately passive — it does not download, stage,
// or swap binaries. Those responsibilities belong to the supervisor
// and will grow in subsequent iterations of architecture/refactor.md
// §9. Iteration 2 only promises that "the UI knows the truth about
// what the channel advertises, and refuses to trust unsigned data."
//
// The public key material used for verification is embedded directly
// from devkey.pub (see keys.go). Since v0.1.0-rc.1 the embedded key
// is the production signing key (ID FD412C2F6CA2B447); the private
// half lives only in the maintainer's password manager and in the
// release workflow's MINISIGN_PRIVATE_KEY secret. Rotating this key
// after release is Decision #6 in architecture/refactor.md §10 and
// requires a client-side rotation protocol that does not yet exist,
// so the key is considered permanent for now.
package updater
