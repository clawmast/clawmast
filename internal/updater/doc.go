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
// from devkey.pub (see keys.go). The current embedded key is a
// development key; rotation to the production key is Decision #6 in
// architecture/refactor.md §10 and is tracked separately.
package updater
