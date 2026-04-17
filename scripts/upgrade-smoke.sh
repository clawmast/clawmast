#!/usr/bin/env bash
# T0-10 — operator-driven upgrade smoke test.
#
# Proves the end-to-end Iteration 0 upgrade path from an operator's
# point of view, without the auto-updater that Iteration 2 will add:
#
#   1. Build worker A with an embedded version label "0.0.1-smoke-a"
#      and the supervisor, staged under a throwaway directory.
#   2. Run install.sh --bin-dir STAGING --version v0.0.1 to lay A down
#      as versions/v0.0.1 and point current -> versions/v0.0.1.
#   3. Boot clawmastd against the install tree.
#   4. Hit /api/version on the live worker; assert it is A.
#   5. Build worker B with version label "0.0.2-smoke-b" into STAGING.
#   6. Run install.sh --bin-dir STAGING --version v0.0.2 so the script
#      rotates the symlinks (architecture/refactor.md §5: atomic
#      current<->previous).
#   7. SIGTERM the worker via run/worker.pid. clawmastd reaps it as
#      ClassUnexpected (protocol §4 edge rule), respawns, and
#      ResolveWorker re-reads current so the new child is binary B.
#   8. Poll /api/version until it reports B. Assert current/previous
#      symlinks and that history.json recorded both spawns.
#
# Full auto-update (fetch + minisign verify + worker-initiated swap +
# IPC restart) lands in Iteration 2. This smoke simulates its final
# step — the supervisor-driven respawn across a new symlink target —
# which is the piece the v1.0 supervisor protocol must already handle.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PREFIX="$(mktemp -d /tmp/cmup.XXXX)"
STAGING="${PREFIX}/staging"
mkdir -p "${STAGING}"
PORT=17092

SUP=""
cleanup() {
  if [[ -n "${SUP}" ]] && kill -0 "${SUP}" 2>/dev/null; then
    kill -TERM "${SUP}" 2>/dev/null || true
    wait "${SUP}" 2>/dev/null || true
  fi
  rm -rf "${PREFIX}"
}
trap cleanup EXIT

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
  curl -sf --max-time 2 "http://127.0.0.1:${PORT}/api/version" || true
}

wait_version_contains() {
  local want="$1" tries="${2:-100}" got=""
  for _ in $(seq 1 "${tries}"); do
    got="$(fetch_version)"
    if [[ "${got}" == *"${want}"* ]]; then
      echo "${got}"
      return 0
    fi
    sleep 0.1
  done
  echo "FAIL: timed out waiting for version containing '${want}'; last payload: ${got}" >&2
  return 1
}

echo "[up] staging supervisor + worker A into ${STAGING}"
go build -o "${STAGING}/clawmastd" ./cmd/clawmastd
build_worker "0.0.1-smoke-a" "${STAGING}/clawmast"

echo "[up] install A as v0.0.1"
bash "${SCRIPT_DIR}/install.sh" \
  --prefix "${PREFIX}" --version v0.0.1 \
  --bin-dir "${STAGING}" --no-service >/dev/null

echo "[up] boot clawmastd (HTTP :${PORT})"
CLAWMAST_HTTP_ADDR="127.0.0.1:${PORT}" \
  "${PREFIX}/bin/clawmastd" -install-root "${PREFIX}" \
  > "${PREFIX}/logs/clawmastd.log" 2>&1 &
SUP=$!

echo "[up] waiting for worker A (v=0.0.1-smoke-a)"
A_OUT="$(wait_version_contains "0.0.1-smoke-a")"
echo "[up] A live: ${A_OUT}"

echo "[up] staging worker B and installing as v0.0.2"
build_worker "0.0.2-smoke-b" "${STAGING}/clawmast"
bash "${SCRIPT_DIR}/install.sh" \
  --prefix "${PREFIX}" --version v0.0.2 \
  --bin-dir "${STAGING}" --no-service >/dev/null

CUR="$(readlink "${PREFIX}/current")"
PREV="$(readlink "${PREFIX}/previous")"
echo "[up] after rotate: current=${CUR} previous=${PREV}"
[[ "${CUR}"  == "versions/v0.0.2" ]] || { echo "FAIL: current=${CUR}";  exit 1; }
[[ "${PREV}" == "versions/v0.0.1" ]] || { echo "FAIL: previous=${PREV}"; exit 1; }

# Simulate the worker-initiated restart the real auto-update flow will
# perform. SIGTERM the worker; clawmastd reaps, re-reads current, and
# respawns the newly installed binary.
WORKER_PID="$(cat "${PREFIX}/run/worker.pid")"
echo "[up] SIGTERM worker pid=${WORKER_PID} to trigger respawn"
kill -TERM "${WORKER_PID}"

echo "[up] waiting for respawned worker B (v=0.0.2-smoke-b)"
B_OUT="$(wait_version_contains "0.0.2-smoke-b" 150)"
echo "[up] B live: ${B_OUT}"

grep -q '"event":"spawn","version":"v0.0.1"' "${PREFIX}/state/history.json" \
  || { echo "FAIL: no v0.0.1 spawn in history"; exit 1; }
grep -q '"event":"spawn","version":"v0.0.2"' "${PREFIX}/state/history.json" \
  || { echo "FAIL: no v0.0.2 spawn in history"; exit 1; }

echo "[up] graceful shutdown"
kill -TERM "${SUP}"
wait "${SUP}"
EXIT=$?
SUP=""
if [[ "${EXIT}" -ne 0 ]]; then
  echo "FAIL: clawmastd exited ${EXIT}"
  tail -30 "${PREFIX}/logs/clawmastd.log"
  exit 1
fi

echo "[up] history tail:"
tail -6 "${PREFIX}/state/history.json"
echo "[up] PASS"
