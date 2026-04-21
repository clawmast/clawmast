#!/usr/bin/env bash
# T2-02 — self-update install smoke test.
#
# End-to-end proof of the auto-update install path:
#
#   1. Build workers A (labelled "0.0.1-smoke-a") and B (labelled
#      "0.0.2-smoke-b") into a throwaway staging dir.
#   2. Tarball B at top-level as "clawmast".
#   3. Generate a throwaway minisign keypair and sign a manifest that
#      advertises B (version v0.0.2, artifact URL, sha256, size).
#   4. Serve the channel (manifest.json + .minisig + artifact.tar.gz)
#      via python3 http.server.
#   5. install.sh --version v0.0.1 lays A down as versions/v0.0.1,
#      symlinks current -> versions/v0.0.1.
#   6. Boot clawmastd with CLAWMAST_UPDATE_URL / _CHANNEL /
#      _PUBKEY_FILE pointed at the local channel.
#   7. Assert /api/version reports A.
#   8. POST /api/updates/install. Assert the response body carries
#      ok:true, version=v0.0.2, current_after=versions/v0.0.2,
#      previous_after=versions/v0.0.1.
#   9. After the handler returns, the worker exits code 0 and the
#      supervisor respawns against the new current link. Assert
#      /api/version eventually reports B.
#  10. Assert on-disk invariants: current -> versions/v0.0.2,
#      previous -> versions/v0.0.1, MANIFEST.json has
#      source=auto-update, history.json records spawns for both.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PREFIX="$(mktemp -d /tmp/cmui.XXXX)"
STAGING="${PREFIX}/staging"
TMP="${PREFIX}/tmp"
CHAN_ROOT="${PREFIX}/channel"
mkdir -p "${STAGING}" "${TMP}" "${CHAN_ROOT}"

WORKER_PORT=17186
CHAN_PORT=17187
SUP_PID=""
SERVER_PID=""

cleanup() {
  for pid in "${SUP_PID}" "${SERVER_PID}"; do
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
    echo "[ui] FAIL: port ${port} is already bound (leaked from a previous run?)" >&2
    echo "[ui]   try: lsof -ti:${port} | xargs kill -KILL" >&2
    exit 1
  fi
}
require_port_free "${WORKER_PORT}"
require_port_free "${CHAN_PORT}"

cd "${REPO_ROOT}"

build_worker() {
  local label="$1" out="$2"
  go build \
    -ldflags "-s -w \
      -X github.com/clawmast/clawmast/internal/version.Version=${label} \
      -X github.com/clawmast/clawmast/internal/version.Commit=${label} \
      -X github.com/clawmast/clawmast/internal/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o "${out}" ./cmd/clawmast
}

fetch_version() {
  curl -sf --max-time 2 "http://127.0.0.1:${WORKER_PORT}/api/version" || true
}

wait_version_contains() {
  local want="$1" tries="${2:-150}" got=""
  for _ in $(seq 1 "${tries}"); do
    got="$(fetch_version)"
    if [[ "${got}" == *"${want}"* ]]; then
      echo "${got}"
      return 0
    fi
    sleep 0.1
  done
  echo "FAIL: timed out waiting for version '${want}'; last payload: ${got}" >&2
  return 1
}

echo "[ui] building supervisor, release tool, workers A+B"
go build -o "${STAGING}/clawmastd" ./cmd/clawmastd
go build -o "${TMP}/clawmast-release" ./cmd/clawmast-release
build_worker "0.0.1-smoke-a" "${STAGING}/clawmast"

echo "[ui] installing A as v0.0.1"
bash "${SCRIPT_DIR}/install.sh" \
  --prefix "${PREFIX}" --version v0.0.1 \
  --bin-dir "${STAGING}" --no-service >/dev/null

echo "[ui] building B into ${TMP}/b/ and tarballing"
mkdir -p "${TMP}/b"
build_worker "0.0.2-smoke-b" "${TMP}/b/clawmast"
(cd "${TMP}/b" && tar -czf "${CHAN_ROOT}/clawmast-v0.0.2.tar.gz" clawmast)

ARTIFACT_SHA="$(shasum -a 256 "${CHAN_ROOT}/clawmast-v0.0.2.tar.gz" | awk '{print $1}')"
ARTIFACT_SIZE="$(wc -c < "${CHAN_ROOT}/clawmast-v0.0.2.tar.gz" | tr -d ' ')"
OS="$(go env GOOS)"
ARCH="$(go env GOARCH)"

echo "[ui] generating throwaway minisign keypair"
PUB="${TMP}/channel.pub"
KEY="${TMP}/channel.key"
"${TMP}/clawmast-release" keygen -pub "${PUB}" > "${KEY}"
chmod 600 "${KEY}"

mkdir -p "${CHAN_ROOT}/stable"
cat > "${CHAN_ROOT}/stable/manifest.json" <<JSON
{
  "channel": "stable",
  "version": "v0.0.2",
  "published_at": "2026-04-20T12:00:00Z",
  "notes": "update-install-smoke test release",
  "artifacts": [
    {
      "os": "${OS}", "arch": "${ARCH}",
      "url": "http://127.0.0.1:${CHAN_PORT}/clawmast-v0.0.2.tar.gz",
      "size": ${ARTIFACT_SIZE},
      "sha256": "${ARTIFACT_SHA}"
    }
  ]
}
JSON
"${TMP}/clawmast-release" sign \
  -key "${KEY}" -in "${CHAN_ROOT}/stable/manifest.json" \
  -out "${CHAN_ROOT}/stable/manifest.json.minisig"

echo "[ui] starting local channel HTTP server on :${CHAN_PORT}"
(cd "${CHAN_ROOT}" && exec python3 -m http.server "${CHAN_PORT}") >"${PREFIX}/chan.log" 2>&1 &
SERVER_PID=$!


echo "[ui] booting clawmastd on :${WORKER_PORT}"
CLAWMAST_HTTP_ADDR="127.0.0.1:${WORKER_PORT}" \
CLAWMAST_UPDATE_URL="http://127.0.0.1:${CHAN_PORT}" \
CLAWMAST_UPDATE_CHANNEL="stable" \
CLAWMAST_UPDATE_PUBKEY_FILE="${PUB}" \
  "${PREFIX}/bin/clawmastd" -install-root "${PREFIX}" \
  > "${PREFIX}/logs/clawmastd.log" 2>&1 &
SUP_PID=$!

echo "[ui] waiting for worker A (0.0.1-smoke-a)"
A_OUT="$(wait_version_contains "0.0.1-smoke-a")"
echo "[ui] A live: ${A_OUT}"

echo "[ui] POST /api/updates/install"
install_resp="$(curl -s -X POST "http://127.0.0.1:${WORKER_PORT}/api/updates/install")"
echo "    ${install_resp}"
echo "${install_resp}" | grep -q '"ok":true'                      || { echo "[ui] FAIL: ok != true"; exit 1; }
echo "${install_resp}" | grep -q '"version":"v0.0.2"'             || { echo "[ui] FAIL: version != v0.0.2"; exit 1; }
echo "${install_resp}" | grep -q '"current_after":"versions/v0.0.2"' || { echo "[ui] FAIL: current_after != versions/v0.0.2"; exit 1; }
echo "${install_resp}" | grep -q '"previous_after":"versions/v0.0.1"' || { echo "[ui] FAIL: previous_after != versions/v0.0.1"; exit 1; }
echo "${install_resp}" | grep -q '"restart_requested":true'       || { echo "[ui] FAIL: restart_requested != true"; exit 1; }

echo "[ui] waiting for respawned worker B (0.0.2-smoke-b)"
B_OUT="$(wait_version_contains "0.0.2-smoke-b" 200)"
echo "[ui] B live: ${B_OUT}"

CUR="$(readlink "${PREFIX}/current")"
PREV="$(readlink "${PREFIX}/previous")"
[[ "${CUR}"  == "versions/v0.0.2" ]] || { echo "[ui] FAIL: current=${CUR}";  exit 1; }
[[ "${PREV}" == "versions/v0.0.1" ]] || { echo "[ui] FAIL: previous=${PREV}"; exit 1; }

grep -q '"source": "auto-update"' "${PREFIX}/versions/v0.0.2/MANIFEST.json" \
  || { echo "[ui] FAIL: v0.0.2 MANIFEST.json missing source=auto-update"; cat "${PREFIX}/versions/v0.0.2/MANIFEST.json"; exit 1; }
grep -q '"event":"spawn","version":"v0.0.1"' "${PREFIX}/state/history.json" \
  || { echo "[ui] FAIL: no v0.0.1 spawn in history"; exit 1; }
grep -q '"event":"spawn","version":"v0.0.2"' "${PREFIX}/state/history.json" \
  || { echo "[ui] FAIL: no v0.0.2 spawn in history"; exit 1; }

echo "[ui] graceful shutdown"
kill -TERM "${SUP_PID}"
wait "${SUP_PID}"
EXIT=$?
SUP_PID=""
if [[ "${EXIT}" -ne 0 ]]; then
  echo "[ui] FAIL: clawmastd exited ${EXIT}"
  tail -40 "${PREFIX}/logs/clawmastd.log"
  exit 1
fi

echo "[ui] history tail:"
tail -6 "${PREFIX}/state/history.json"
echo "[ui] PASS"
