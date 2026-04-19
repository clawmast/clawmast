# ClawMast build commands.

SHELL := /usr/bin/env bash
GO    := go

# Version metadata injected via -ldflags.
VERSION   ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo "dev")
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILDTIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X github.com/clawmast/clawmast/internal/version.Version=$(VERSION) \
	-X github.com/clawmast/clawmast/internal/version.Commit=$(COMMIT) \
	-X github.com/clawmast/clawmast/internal/version.BuildTime=$(BUILDTIME)

.PHONY: build build-worker build-supervisor build-release test lint vet tidy snapshot smoke install install-smoke rollback-smoke upgrade-smoke blacklist-smoke update-smoke update-install-smoke update-health-smoke release-smoke supervisor-drill playground-up playground-down playground-status playground-publish playground-nuke clean

build: build-worker build-supervisor build-release

build-worker:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/clawmast ./cmd/clawmast

build-supervisor:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/clawmastd ./cmd/clawmastd

build-release:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/clawmast-release ./cmd/clawmast-release

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

lint: vet

tidy:
	$(GO) mod tidy

snapshot:
	goreleaser build --snapshot --clean

# Smoke test: verify built binaries run and that ldflags version metadata
# was injected (i.e. Commit is not the default "none" and BuildTime is not
# the default "unknown"). Run in CI after `make build` on each platform.
smoke: build
	@echo "== smoke: clawmast version =="
	@./bin/clawmast version
	@./bin/clawmast version | grep -Eqv 'none|unknown' \
		|| { echo "SMOKE FAIL: clawmast ldflags fallback detected"; exit 1; }
	@echo "== smoke: clawmastd version =="
	@./bin/clawmastd version
	@./bin/clawmastd version | grep -Eqv 'none|unknown' \
		|| { echo "SMOKE FAIL: clawmastd ldflags fallback detected"; exit 1; }
	@echo "== SMOKE OK =="

# Run scripts/install.sh against the local checkout. Use CLAWMAST_HOME to
# override the default ~/.clawmast install root.
install: build
	bash scripts/install.sh --source local

# CI-friendly smoke test: installs into a throwaway prefix with --no-service
# and verifies the on-disk layout + a clawmastd boot/shutdown round-trip.
install-smoke:
	bash scripts/install-smoke.sh

# T0-08 — forced-failure rollback test. Boots clawmastd with a deliberately
# crashing worker as "current" and verifies the supervisor swaps current ↔
# previous after the crash-loop budget is exhausted.
rollback-smoke:
	bash scripts/rollback-smoke.sh

# T0-10 — operator-driven upgrade smoke. Installs worker A, rotates to B via
# install.sh, and verifies clawmastd respawns into B across the new symlink.
upgrade-smoke:
	bash scripts/upgrade-smoke.sh

# T1-03 — version blacklist smoke. Installs two good versions, then uses
# POST /api/blacklist to mark the current one as bad and verifies the
# supervisor rolls back to the previous version.
blacklist-smoke:
	bash scripts/blacklist-smoke.sh

# T2-01 — update channel & signature verification smoke. Generates a
# throwaway minisign keypair, signs a test manifest, serves it over a
# local HTTP server, and asserts POST /api/updates/check verifies the
# signature (happy path + tampering).
update-smoke:
	bash scripts/update-smoke.sh

# T2-02 — self-update install smoke. Installs worker A, serves a signed
# manifest advertising worker B over a local HTTP server, POSTs
# /api/updates/install, and asserts the supervisor respawns into B with
# current/previous rotated correctly.
update-install-smoke:
	bash scripts/update-install-smoke.sh

# T2-03 — install HealthGate smoke. Installs a good worker A, installs
# a failstartup-tagged B that crashes before READY, and asserts the
# supervisor's HealthGate rolls back to A, blacklists B with an
# install-health-* reason, and clears the install-gate marker.
update-health-smoke:
	bash scripts/update-health-smoke.sh

# T2-04 — release pipeline smoke. Exercises `clawmast-release build` +
# `manifest` + `sign` end-to-end by feeding the tool's outputs into a
# worker via POST /api/updates/check; asserts the worker accepts a
# signed manifest produced by the real release pipeline (not a
# hand-crafted one).
release-smoke:
	bash scripts/release-smoke.sh

# I3 — supervisor drill covering the Iteration 3 additions that the
# other smoke scripts do not exercise: bearer token persistence,
# /api/openclaw/status schema, supervisor respawn after SIGKILL, and
# graceful SIGTERM on clawmastd.
supervisor-drill:
	bash scripts/supervisor-drill.sh

# Auto-update playground — persistent local environment for clicking
# through the update flow in a real browser. See scripts/playground.sh
# for the full subcommand surface. Unlike the *-smoke targets, this
# does not tear itself down; use `make playground-down` to stop or
# `make playground-nuke` to also wipe the install tree.
playground-up:
	bash scripts/playground.sh up

playground-down:
	bash scripts/playground.sh down

playground-status:
	bash scripts/playground.sh status

# Usage: make playground-publish V=v0.1.1-playground CHAN=stable
playground-publish:
	bash scripts/playground.sh publish $(V) $(CHAN)

playground-nuke:
	bash scripts/playground.sh nuke

clean:
	rm -rf bin/ dist/
