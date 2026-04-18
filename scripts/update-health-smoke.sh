#!/usr/bin/env bash
# T2-03 — install HealthGate smoke test.
#
# Proves the post-install observation window works end-to-end:
#
#   1. Build clawmastd, the release tool, and worker A at v0.0.1
#      (the "known good" version, no failstartup tag).
#   2. install.sh lays A down as versions/v0.0.1.
#   3. Build worker B at v0.0.2 with `-tags failstartup` and wire
#      it through a signed manifest on a local HTTP channel. B's
#      only job is to read CLAWMAST_FAIL_MODE from the supervisor
#      environment and crash before READY.
#   4. Launch clawmastd with CLAWMAST_FAIL_MODE=crash and short
#      gate durations (3 s window, 1 s stable).
#   5. Wait for A to announce itself via /api/version.
#   6. POST /api/updates/install — installs B and triggers a
#      worker restart. B crashes, the gate catches it with
#      GateFailReasonCrash, the supervisor blacklists v0.0.2,
#      swaps current<->previous, and respawns A.
#   7. Assert A is live again and that state on disk carries the
#      blacklist entry, the rollback history event, no leftover
#      install-gate marker, and the reversed symlinks.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PREFIX="$(mktemp -d /tmp/cmhg.XXXX)"
STAGING="${PREFIX}/staging"
TMP="${PREFIX}/tmp"
CHAN_ROOT="${PREFIX}/channel"
mkdir -p "${STAGING}" "${TMP}" "${CHAN_ROOT}"

WORKER_PORT=17188
CHAN_PORT=17189
SUP_PID=""
SERVER_PID=""

cleanup() {
  local rc=$?
  for pid in "${SUP_PID}" "${SERVER_PID}"; do
    if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null; then
      kill -TERM "${pid}" 2>/dev/null || true
      wait "${pid}" 2>/dev/null || true
    fi
  done
  if [[ "${rc}" -ne 0 ]]; then
    echo "[hg] smoke failed; preserving scratch at ${PREFIX}" >&2
    return
  fi
  rm -rf "${PREFIX}"
}
trap cleanup EXIT

require_port_free() {
  local port="$1"
  if lsof -ti:"${port}" >/dev/null 2>&1; then
    echo "[hg] FAIL: port ${port} is already bound" >&2
    exit 1
  fi
}
require_port_free "${WORKER_PORT}"
require_port_free "${CHAN_PORT}"

cd "${REPO_ROOT}"

build_worker() {
  local label="$1" out="$2"
  shift 2
  go build "$@" \
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
  local want="$1" tries="${2:-200}" got=""
  for _ in $(seq 1 "${tries}"); do
    got="$(fetch_version)"
    if [[ "${got}" == *"${want}"* ]]; then
      echo "${got}"
      return 0
    fi
    sleep 0.1
  done
  echo "FAIL: timed out waiting for '${want}'; last: ${got}" >&2
  return 1
}

# wait_history_grep polls history.json for a regex match up to tries
# times, 100 ms apart. We cannot rely on /api/version to change
# shape (A's label is identical before and after rollback) so the
# ledger is the authoritative signal for "the gate ran its course".
wait_history_grep() {
  local pattern="$1" tries="${2:-300}"
  local path="${PREFIX}/state/history.json"
  for _ in $(seq 1 "${tries}"); do
    if [[ -f "${path}" ]] && grep -qE "${pattern}" "${path}"; then
      return 0
    fi
    sleep 0.1
  done
  echo "FAIL: timed out waiting for history match '${pattern}'" >&2
  [[ -f "${path}" ]] && tail -20 "${path}" >&2
  return 1
}

echo "[hg] building clawmastd, release tool, A (good), B (failstartup)"
go build -o "${STAGING}/clawmastd" ./cmd/clawmastd
go build -o "${TMP}/clawmast-release" ./cmd/clawmast-release
build_worker "0.0.1-good" "${STAGING}/clawmast"

echo "[hg] installing A as v0.0.1"
bash "${SCRIPT_DIR}/install.sh" \
  --prefix "${PREFIX}" --version v0.0.1 \
  --bin-dir "${STAGING}" --no-service >/dev/null

mkdir -p "${TMP}/b"
build_worker "0.0.2-bad" "${TMP}/b/clawmast" -tags failstartup
(cd "${TMP}/b" && tar -czf "${CHAN_ROOT}/clawmast-v0.0.2.tar.gz" clawmast)

ARTIFACT_SHA="$(shasum -a 256 "${CHAN_ROOT}/clawmast-v0.0.2.tar.gz" | awk '{print $1}')"
ARTIFACT_SIZE="$(wc -c < "${CHAN_ROOT}/clawmast-v0.0.2.tar.gz" | tr -d ' ')"
OS="$(go env GOOS)"
ARCH="$(go env GOARCH)"

echo "[hg] generating throwaway minisign keypair"
PUB="${TMP}/channel.pub"
KEY="${TMP}/channel.key"
"${TMP}/clawmast-release" keygen -pub "${PUB}" > "${KEY}"
chmod 600 "${KEY}"

cat > "${CHAN_ROOT}/manifest.json" <<JSON
{
  "channel": "stable",
  "version": "v0.0.2",
  "published_at": "2026-04-20T12:00:00Z",
  "notes": "update-health-smoke bad release",
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
  -key "${KEY}" -in "${CHAN_ROOT}/manifest.json" -out "${CHAN_ROOT}/manifest.json.minisig"

echo "[hg] starting local channel on :${CHAN_PORT}"
(cd "${CHAN_ROOT}" && exec python3 -m http.server "${CHAN_PORT}") >"${PREFIX}/chan.log" 2>&1 &
SERVER_PID=$!

echo "[hg] booting clawmastd (gate=3s, stable=1s, fail=crash)"
CLAWMAST_HTTP_ADDR="127.0.0.1:${WORKER_PORT}" \
CLAWMAST_UPDATE_URL="http://127.0.0.1:${CHAN_PORT}" \
CLAWMAST_UPDATE_CHANNEL="stable" \
CLAWMAST_UPDATE_PUBKEY_FILE="${PUB}" \
CLAWMAST_FAIL_MODE="crash" \
CLAWMAST_GATE_WINDOW="3s" \
CLAWMAST_GATE_STABLE_FOR="1s" \
  "${PREFIX}/bin/clawmastd" -install-root "${PREFIX}" \
  > "${PREFIX}/logs/clawmastd.log" 2>&1 &
SUP_PID=$!

echo "[hg] waiting for A (v0.0.1-good)"
A_OUT="$(wait_version_contains "0.0.1-good")"
echo "[hg] A live: ${A_OUT}"

echo "[hg] POST /api/updates/install"
install_resp="$(curl -s -X POST "http://127.0.0.1:${WORKER_PORT}/api/updates/install")"
echo "    ${install_resp}"
echo "${install_resp}" | grep -q '"ok":true' || { echo "[hg] FAIL: install rejected"; exit 1; }
echo "${install_resp}" | grep -q '"version":"v0.0.2"' || { echo "[hg] FAIL: install did not advertise v0.0.2"; exit 1; }

echo "[hg] waiting for gate rollback to land in history.json"
wait_history_grep '"event":"rollback","version":"v0.0.2"' 300
echo "[hg] waiting for A to re-answer /api/version"
A_OUT2="$(wait_version_contains "0.0.1-good" 300)"
echo "[hg] A respawned: ${A_OUT2}"

CUR="$(readlink "${PREFIX}/current")"
PREV="$(readlink "${PREFIX}/previous")"
[[ "${CUR}"  == "versions/v0.0.1" ]] || { echo "[hg] FAIL: current=${CUR}, expected versions/v0.0.1"; exit 1; }
[[ "${PREV}" == "versions/v0.0.2" ]] || { echo "[hg] FAIL: previous=${PREV}, expected versions/v0.0.2"; exit 1; }

if [[ -f "${PREFIX}/state/install-gate.json" ]]; then
  echo "[hg] FAIL: install-gate.json still exists after rollback"
  exit 1
fi

if ! grep -qE '"version":[[:space:]]*"v0\.0\.2"' "${PREFIX}/state/blacklist.json"; then
  echo "[hg] FAIL: v0.0.2 not in blacklist"
  cat "${PREFIX}/state/blacklist.json"
  exit 1
fi
if ! grep -qE '"reason":[[:space:]]*"install-health-(crash|timeout)"' "${PREFIX}/state/blacklist.json"; then
  echo "[hg] FAIL: blacklist entry missing install-health-* reason"
  cat "${PREFIX}/state/blacklist.json"
  exit 1
fi

if ! grep -q '"event":"rollback","version":"v0.0.2"' "${PREFIX}/state/history.json"; then
  echo "[hg] FAIL: no rollback event for v0.0.2 in history"
  tail -20 "${PREFIX}/state/history.json"
  exit 1
fi

echo "[hg] graceful shutdown"
kill -TERM "${SUP_PID}"
wait "${SUP_PID}"
EXIT=$?
SUP_PID=""
if [[ "${EXIT}" -ne 0 ]]; then
  echo "[hg] FAIL: clawmastd exited ${EXIT}"
  tail -40 "${PREFIX}/logs/clawmastd.log"
  exit 1
fi

echo "[hg] history tail:"
tail -6 "${PREFIX}/state/history.json"
echo "[hg] PASS"
