# AGENTS.md

Guidance for AI coding agents and human contributors working on `clawmast`.

Keep this file short and high-signal. When in doubt, prefer the rules here over
general habits, and update this file when a new cross-cutting decision is made.

## Project rules

### R1. Prefer the latest stable versions

When introducing or upgrading **any** dependency, use the newest stable
release available at the time of the change. This applies to, at minimum:

- Go toolchain (`go` directive in `go.mod`, `toolchain` directive if pinned)
- Go module dependencies (`go get pkg@latest` for new deps; keep `go.sum` current)
- Node / pnpm / npm packages under `web/` (and any other JS workspace we add)
- GitHub Actions (`actions/*@vN`, third-party actions — pin to the latest
  major, use SHA pinning only when required by security policy)
- Base images in any `Dockerfile` (use the newest LTS tag, not `latest`)
- Linters, formatters, and test runners (golangci-lint, biome, vitest, etc.)

**Only deviate when**:

1. A documented incompatibility blocks the upgrade (cite the upstream issue or
   release note in the commit body).
2. A hard external constraint forces an older version (e.g. a target OS
   ships an older library; record the constraint in the commit body).
3. The newest release is a pre-release / RC and we have no business reason to
   adopt it early.

When you skip an upgrade, say so explicitly in the PR description so the next
agent doesn't re-try and re-discover the same incompatibility.

### R2. Supervisor / worker split is non-negotiable

Crash-resilience is the whole product. Never fold supervisor logic into the
worker binary or vice versa.

### R3. Language policy

- Code, comments, identifiers, commit messages, and PR titles/descriptions
  are **English**.
- Chat with the primary maintainer may be in Chinese; translate to English
  before anything lands in the repo.

### R4. Commit identity

Commits are authored as `teomyth <207293165+teomyth@users.noreply.github.com>`.
Never commit from a different email that would expose a private address.

### R5. Platform tiers — keep the worker Windows-compilable

Tier 1 (macOS, Linux) carries the full stack: `clawmastd` supervisor, worker,
auto-update, installer. Tier 2 (Windows) is **worker-only, manual install**.

The implication for contributors:

- The `clawmast` worker must keep compiling under `GOOS=windows GOARCH=amd64`.
  CI does not build Windows, but `go vet` and `go build` for `windows/amd64`
  must succeed locally before merging changes under `cmd/clawmast/` or
  `internal/` that are not explicitly POSIX-only.
- POSIX-only code (supervisor spawning, launchd / systemd glue, unix sockets,
  PTY, signal handling) belongs under build tags `//go:build unix` or in
  packages referenced only by `cmd/clawmastd/`. The worker may depend on
  POSIX packages *behind an interface* with a `//go:build windows` stub that
  returns `errors.ErrUnsupported`.
- `cmd/clawmastd/` and its supporting packages under `internal/supervisor/`
  are **not** required to compile on Windows.

Windows parity (supervisor, auto-update, tray) is deferred until after v1.0.0.

### R6. Push policy

Local commits stay local until one of these triggers fires:

- a tag is being cut for release (e.g. `vX.Y.Z`)
- a workflow that consumes the remote (auto-update smoke, release-pipeline
  dry run, cross-machine smoke) needs the commit on GitHub
- the maintainer explicitly asks to push

Otherwise, leave the branch ahead of origin. Do not offer to push "to be
safe" — the maintainer prefers a quiet working tree and audits the
unpushed log before pushing in a batch.

## How to propose a rule change

Open a PR that edits this file and explains the rationale in the description.
Do not add rules here unilaterally during unrelated work.
