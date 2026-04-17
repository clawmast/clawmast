#!/usr/bin/env bash
# T2-01 — update channel & signature verification smoke test.
#
# Generates a throwaway minisign keypair, signs a fake manifest with
# it, serves the manifest + .minisig from a local HTTP server, boots
# the worker with CLAWMAST_UPDATE_URL pointing at that server and
# CLAWMAST_UPDATE_PUBKEY_FILE pointing at the test public key, then
# hits POST /api/updates/check and asserts:
#
#   1. source == "signed-manifest"     (happy path)
#   2. latest == the version we put in the manifest
#   3. update_available == true
#   4. tampering the manifest flips source to "error" with
#      error_code == "bad-signature"
#
# Covers architecture/refactor.md §6 (signature hardening) for
# Iteration 2.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PREFIX="$(mktemp -d /tmp/cmup.XXXX)"
# Fixed ports are fine for a local smoke, but we must fail loudly if
# they are already bound — otherwise a leaked server from a previous
# run silently serves the wrong manifest and every assertion lies.
WORKER_PORT=17184
CHAN_PORT=17185
WORKER_PID=""
SERVER_PID=""
trap 'cleanup' EXIT
cleanup() {
  for pid in "${WORKER_PID}" "${SERVER_PID}"; do
    if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null; then
      kill -TERM "${pid}" 2>/dev/null || true
      wait "${pid}" 2>/dev/null || true
    fi
  done
  rm -rf "${PREFIX}"
}

require_port_free() {
  local port="$1"
  if lsof -ti:"${port}" >/dev/null 2>&1; then
    echo "[up] FAIL: port ${port} is already bound (leaked from a previous run?)" >&2
    echo "[up]   try: lsof -ti:${port} | xargs kill -KILL" >&2
    exit 1
  fi
}
require_port_free "${WORKER_PORT}"
require_port_free "${CHAN_PORT}"

cd "${REPO_ROOT}"

echo "[up] building binaries"
go build -o "${PREFIX}/clawmast" ./cmd/clawmast
go build -o "${PREFIX}/clawmast-release" ./cmd/clawmast-release

echo "[up] generating throwaway minisign keypair"
PUB="${PREFIX}/channel.pub"
KEY="${PREFIX}/channel.key"
"${PREFIX}/clawmast-release" keygen -pub "${PUB}" > "${KEY}"
chmod 600 "${KEY}"

echo "[up] building channel root with a manifest"
CHAN_ROOT="${PREFIX}/channel"
mkdir -p "${CHAN_ROOT}"
cat > "${CHAN_ROOT}/manifest.json" <<'JSON'
{
  "channel": "stable",
  "version": "v9.9.9",
  "published_at": "2026-04-20T12:00:00Z",
  "notes": "update-smoke test release",
  "artifacts": [
    {
      "os": "darwin", "arch": "arm64",
      "url": "https://example.com/clawmast-darwin-arm64.tar.gz",
      "size": 1024,
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    }
  ]
}
JSON
"${PREFIX}/clawmast-release" sign \
  -key "${KEY}" -in "${CHAN_ROOT}/manifest.json" -out "${CHAN_ROOT}/manifest.json.minisig"

echo "[up] starting local channel HTTP server on port ${CHAN_PORT}"
# `exec` replaces the subshell with python so SERVER_PID is the actual
# python PID. Without this, TERM hits the intermediate shell and python
# gets reparented to init (PPID=1), holding the port across runs.
(cd "${CHAN_ROOT}" && exec python3 -m http.server "${CHAN_PORT}") >"${PREFIX}/chan.log" 2>&1 &
SERVER_PID=$!

echo "[up] starting worker on port ${WORKER_PORT}"
CLAWMAST_HTTP_ADDR="127.0.0.1:${WORKER_PORT}" \
CLAWMAST_UPDATE_URL="http://127.0.0.1:${CHAN_PORT}" \
CLAWMAST_UPDATE_CHANNEL="stable" \
CLAWMAST_UPDATE_PUBKEY_FILE="${PUB}" \
  "${PREFIX}/clawmast" >"${PREFIX}/worker.log" 2>&1 &
WORKER_PID=$!

echo "[up] waiting for worker health"
for _ in $(seq 1 50); do
  if curl -s -o /dev/null "http://127.0.0.1:${WORKER_PORT}/api/health"; then
    break
  fi
  sleep 0.1
done

echo "[up] POST /api/updates/check (happy path)"
resp="$(curl -s -X POST "http://127.0.0.1:${WORKER_PORT}/api/updates/check")"
echo "    ${resp}"
echo "${resp}" | grep -q '"source":"signed-manifest"' || { echo "[up] FAIL: source != signed-manifest"; exit 1; }
echo "${resp}" | grep -q '"latest":"v9.9.9"'         || { echo "[up] FAIL: latest != v9.9.9"; exit 1; }
echo "${resp}" | grep -q '"update_available":true'   || { echo "[up] FAIL: update_available != true"; exit 1; }
echo "${resp}" | grep -q '"channel":"stable"'        || { echo "[up] FAIL: channel != stable"; exit 1; }

echo "[up] tampering manifest (flip version) to force signature failure"
sed -i.bak 's/v9.9.9/v8.8.8/' "${CHAN_ROOT}/manifest.json"

resp="$(curl -s -X POST "http://127.0.0.1:${WORKER_PORT}/api/updates/check")"
echo "    ${resp}"
echo "${resp}" | grep -q '"source":"error"'                  || { echo "[up] FAIL: tampered source != error"; exit 1; }
echo "${resp}" | grep -q '"error_code":"bad-signature"'      || { echo "[up] FAIL: tampered error_code != bad-signature"; exit 1; }
echo "${resp}" | grep -q '"update_available":false'          || { echo "[up] FAIL: tampered update_available != false"; exit 1; }

echo "[up] restoring manifest and re-checking (happy path again)"
mv "${CHAN_ROOT}/manifest.json.bak" "${CHAN_ROOT}/manifest.json"

resp="$(curl -s -X POST "http://127.0.0.1:${WORKER_PORT}/api/updates/check")"
echo "    ${resp}"
echo "${resp}" | grep -q '"source":"signed-manifest"' || { echo "[up] FAIL: source after restore != signed-manifest"; exit 1; }

echo "[up] PASS"
