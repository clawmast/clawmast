#!/usr/bin/env bash
# T2-04 — release pipeline smoke test.
#
# Proves the `clawmast-release` build+manifest+sign subcommands produce
# outputs the updater client accepts end-to-end, without going anywhere
# near GitHub Actions:
#
#   1. Build clawmast-release.
#   2. `clawmast-release build` cross-compiles a worker tarball for the
#      current host into ${DIST}/clawmast-<os>-<arch>.tar.gz.
#   3. `clawmast-release keygen` mints a throwaway minisign keypair.
#   4. `clawmast-release manifest` scans ${DIST}, hashes the tarball,
#      and writes a manifest.json rooted at http://127.0.0.1:${CHAN}.
#   5. `clawmast-release sign` produces manifest.json.minisig.
#   6. python3 http.server serves the channel on ${CHAN}.
#   7. Boot the worker tarball's binary with CLAWMAST_UPDATE_URL and
#      CLAWMAST_UPDATE_PUBKEY_FILE pointed at the channel; assert
#      POST /api/updates/check returns source=signed-manifest and
#      latest matches the manifest version.
#
# Failure modes the script must catch:
#   - build subcommand produces tarballs the manifest loader rejects
#   - manifest subcommand generates fields Validate() refuses
#   - sign output is not consumable by updater.Client

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PREFIX="$(mktemp -d /tmp/cmrs.XXXX)"
DIST="${PREFIX}/dist"
CHAN_ROOT="${PREFIX}/channel"
mkdir -p "${DIST}" "${CHAN_ROOT}"

WORKER_PORT=17188
CHAN_PORT=17189
WORKER_PID=""
SERVER_PID=""

cleanup() {
  for pid in "${WORKER_PID}" "${SERVER_PID}"; do
    if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null; then
      kill -TERM "${pid}" 2>/dev/null || true
      wait "${pid}" 2>/dev/null || true
    fi
  done
  rm -rf "${PREFIX}"
}
trap cleanup EXIT

require_port_free() {
  local port="$1"
  if lsof -ti:"${port}" >/dev/null 2>&1; then
    echo "[rs] FAIL: port ${port} is already bound (leaked from a previous run?)" >&2
    echo "[rs]   try: lsof -ti:${port} | xargs kill -KILL" >&2
    exit 1
  fi
}
require_port_free "${WORKER_PORT}"
require_port_free "${CHAN_PORT}"

cd "${REPO_ROOT}"

OS="$(go env GOOS)"
ARCH="$(go env GOARCH)"
VERSION="v0.0.0-release-smoke"

echo "[rs] building release tool"
go build -o "${PREFIX}/clawmast-release" ./cmd/clawmast-release

echo "[rs] running clawmast-release build (host: ${OS}/${ARCH})"
"${PREFIX}/clawmast-release" build \
  -out "${DIST}" \
  -version "${VERSION}" \
  -commit "releasesmoke" \
  -platforms "${OS}/${ARCH}"
TARBALL="${DIST}/clawmast-${OS}-${ARCH}.tar.gz"
[[ -f "${TARBALL}" ]] || { echo "[rs] FAIL: tarball missing at ${TARBALL}"; exit 1; }

echo "[rs] minting throwaway minisign keypair"
PUB="${PREFIX}/channel.pub"
KEY="${PREFIX}/channel.key"
"${PREFIX}/clawmast-release" keygen -pub "${PUB}" > "${KEY}"
chmod 600 "${KEY}"

echo "[rs] generating manifest pinned at http://127.0.0.1:${CHAN_PORT}"
"${PREFIX}/clawmast-release" manifest \
  -dir "${DIST}" \
  -out "${CHAN_ROOT}/manifest.json" \
  -channel stable \
  -version "${VERSION}" \
  -base-url "http://127.0.0.1:${CHAN_PORT}" \
  -notes "release-smoke ${VERSION}"

# The manifest references tarball by filename only; copy it into the
# channel root so the base-URL + filename join actually resolves.
cp "${TARBALL}" "${CHAN_ROOT}/"

echo "[rs] signing manifest"
"${PREFIX}/clawmast-release" sign \
  -key "${KEY}" \
  -in "${CHAN_ROOT}/manifest.json" \
  -out "${CHAN_ROOT}/manifest.json.minisig" \
  -comment "${VERSION}"
ls -la "${CHAN_ROOT}"

echo "[rs] starting channel server on :${CHAN_PORT}"
(cd "${CHAN_ROOT}" && exec python3 -m http.server "${CHAN_PORT}") >"${PREFIX}/chan.log" 2>&1 &
SERVER_PID=$!

echo "[rs] unpacking worker tarball to boot it"
tar -xzf "${TARBALL}" -C "${PREFIX}"
[[ -x "${PREFIX}/clawmast" ]] || { echo "[rs] FAIL: extracted binary not executable"; exit 1; }

echo "[rs] starting worker on :${WORKER_PORT}"
CLAWMAST_HTTP_ADDR="127.0.0.1:${WORKER_PORT}" \
CLAWMAST_UPDATE_URL="http://127.0.0.1:${CHAN_PORT}" \
CLAWMAST_UPDATE_CHANNEL="stable" \
CLAWMAST_UPDATE_PUBKEY_FILE="${PUB}" \
  "${PREFIX}/clawmast" >"${PREFIX}/worker.log" 2>&1 &
WORKER_PID=$!

echo "[rs] waiting for worker health"
for _ in $(seq 1 50); do
  if curl -s -o /dev/null "http://127.0.0.1:${WORKER_PORT}/api/health"; then break; fi
  sleep 0.1
done

echo "[rs] POST /api/updates/check"
resp="$(curl -s -X POST "http://127.0.0.1:${WORKER_PORT}/api/updates/check")"
echo "    ${resp}"

echo "${resp}" | grep -q '"source":"signed-manifest"' || { echo "[rs] FAIL: source != signed-manifest"; exit 1; }
echo "${resp}" | grep -q "\"latest\":\"${VERSION}\""  || { echo "[rs] FAIL: latest != ${VERSION}";     exit 1; }
echo "${resp}" | grep -q '"channel":"stable"'         || { echo "[rs] FAIL: channel != stable";       exit 1; }
# The smoke tarball's embedded version label matches VERSION, so
# update_available should be false (running == latest). This proves
# the manifest's Version field lines up with the ldflags-injected
# version in the tarball that the same pipeline produced.
echo "${resp}" | grep -q '"update_available":false'   || { echo "[rs] FAIL: update_available != false (running==latest)"; exit 1; }

echo "[rs] PASS"
