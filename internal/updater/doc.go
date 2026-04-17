// Package updater fetches release manifests from a configured channel
// (GitHub Releases or Cloudflare R2 at update.clawmast.com), verifies
// minisign signatures, and stages new versions under ~/.clawmast/versions/
// for the supervisor to activate.
//
// Populated in T0-08 and T0-09.
package updater
