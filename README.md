# ClawMast

**Local-first recovery console for OpenClaw.**

> When OpenClaw is down, ClawMast is still there.

ClawMast is an out-of-band management agent that runs on the same host as
OpenClaw. It exposes a phone-friendly web UI to inspect status, restart the
Gateway, rotate provider keys, and tail logs — even when OpenClaw itself is
unhealthy.

## Status

This repository is in the **walking-skeleton** phase: the Go module
layout, supervisor/worker split, and build/versioning wiring are in place,
but most behaviour is still stubbed. Public architecture docs will land
under `docs/` as the design stabilizes.

An earlier Node.js/TypeScript prototype has been archived; the current
implementation is a ground-up rewrite in Go with a supervisor/worker
process model and self-update.

## Architecture

- **`clawmastd`** — supervisor. Spawns and supervises the worker,
  health-checks it, performs atomic updates and rollback. stdlib-only,
  intentionally frozen.
- **`clawmast`** — worker. HTTP API, embedded web UI, PTY, gateway
  orchestration, optional cloud tunnel. UI will ship with built-in
  translations for Chinese, English, Japanese, Korean, and French.
- **Self-update** — channel manifest with signed artifacts and an atomic
  symlink switch for rollback.
- **Cloud (optional)** — outbound WebSocket tunnel to `cloud.clawmast.com`
  for remote recovery when the local host is unreachable.

## Build

```bash
make build       # both binaries into ./bin/
make test
make lint
```

## License

GNU Affero General Public License v3.0 or later (AGPL-3.0-or-later).
See [`LICENSE`](LICENSE).

## Contributing

Contributions must be submitted under the terms of the ClawMast Contributor
License Agreement. See [`CLA.md`](CLA.md) before opening a pull request.
