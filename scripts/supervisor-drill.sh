#!/usr/bin/env bash
# Iteration 3 supervisor drill.
#
# Covers the paths the other *-smoke.sh scripts do not exercise:
#
#   - The worker persists a fresh 64-char bearer token to
#     <root>/state/token with mode 0600 and logs a "bearer token
#     generated" line with a short fingerprint (agent/auth.go).
#   - /api/openclaw/status responds to a loopback request without
#     credentials and returns a parseable JSON document consistent
#     with openclaw.Manager's zero state (probe may or may not have
#     landed yet; the schema must be valid either way).
#   - Supervisor respawns the worker when the worker process is
#     SIGKILL'd outside the protocol. The respawned worker must
#     accept HTTP again, proving the listener and token paths are
#     idempotent across worker restarts.
#   - Graceful SIGTERM on clawmastd flushes a "stop" history event
#     and exits 0 per supervisor-protocol.md §7.
#
# Runs against a throwaway install root created via scripts/install.sh
# --no-service so it is safe in CI and developer machines. Uses port
# 17093 to avoid colliding with the default 17080 a maintainer may be
# running, and with the other smoke scripts' ports.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PREFIX="$(mktemp -d /tmp/cmdrl.XXXX)"
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

echo "[drill] building binaries"
make build >/dev/null

echo "[drill] installing v0.0.1 at ${PREFIX}"
bash "${SCRIPT_DIR}/install.sh" --prefix "${PREFIX}" --version v0.0.1 --source local --no-service >/dev/null

SMOKE_PORT=17093
echo "[drill] booting clawmastd on :${SMOKE_PORT}"
CLAWMAST_HTTP_ADDR="127.0.0.1:${SMOKE_PORT}" \
  "${PREFIX}/bin/clawmastd" -install-root "${PREFIX}" \
  > "${PREFIX}/logs/clawmastd.log" 2>&1 &
SUP=$!

echo "[drill] waiting for worker ready (up to 8 s)"
for i in $(seq 1 80); do
  grep -q "worker ready" "${PREFIX}/logs/clawmastd.log" 2>/dev/null && break
  sleep 0.1
  if [[ "${i}" == "80" ]]; then
    echo "FAIL: worker never reached ready"
    tail -40 "${PREFIX}/logs/clawmastd.log"
    exit 1
  fi
done

# Step 1 — bearer token persisted with correct permissions.
TOKEN_FILE="${PREFIX}/state/token"
[[ -f "${TOKEN_FILE}" ]] || { echo "FAIL: no token file at ${TOKEN_FILE}"; exit 1; }
MODE=$(stat -f "%A" "${TOKEN_FILE}" 2>/dev/null || stat -c "%a" "${TOKEN_FILE}")
[[ "${MODE}" == "600" ]] || { echo "FAIL: token mode=${MODE}, want 600"; exit 1; }
TOK=$(tr -d '\n' < "${TOKEN_FILE}")
[[ ${#TOK} -eq 64 ]] || { echo "FAIL: token len=${#TOK}, want 64"; exit 1; }
echo "[drill] token ok: mode=600 len=64 fp=${TOK:0:8}"

# Step 2 — boot log announced the token (generated path).
grep -q 'bearer token generated' "${PREFIX}/logs/clawmastd.log" \
  || { echo "FAIL: log missing 'bearer token generated'"; tail -30 "${PREFIX}/logs/clawmastd.log"; exit 1; }

# Step 3 — openclaw status endpoint answers on loopback without a token.
curl -sf "http://127.0.0.1:${SMOKE_PORT}/api/openclaw/status" > "${PREFIX}/logs/oc-status.json" \
  || { echo "FAIL: /api/openclaw/status"; tail -30 "${PREFIX}/logs/clawmastd.log"; exit 1; }
python3 -m json.tool < "${PREFIX}/logs/oc-status.json" > /dev/null \
  || { echo "FAIL: openclaw/status is not valid JSON"; cat "${PREFIX}/logs/oc-status.json"; exit 1; }
# The Manager's zero state exposes these fields; schema changes must be
# coordinated with the frontend renderer in internal/embed/dist/app.js.
for field in '"probed"' '"alive"' '"cli_missing"' '"sessions_count"' '"channel_count"'; do
  grep -q "${field}" "${PREFIX}/logs/oc-status.json" \
    || { echo "FAIL: openclaw/status missing field ${field}"; cat "${PREFIX}/logs/oc-status.json"; exit 1; }
done
echo "[drill] openclaw/status schema ok"

# Step 4 — SIGKILL the worker; supervisor must respawn it.
WORKER_PID=$(pgrep -P "${SUP}" -f 'versions/v0.0.1/clawmast' | head -1 || true)
[[ -n "${WORKER_PID}" ]] || { echo "FAIL: no worker child of supervisor"; ps -f -p "${SUP}"; exit 1; }
kill -9 "${WORKER_PID}"
echo "[drill] killed worker ${WORKER_PID}; waiting for respawn"
NEW_WORKER=""
for i in $(seq 1 50); do
  NEW_WORKER=$(pgrep -P "${SUP}" -f 'versions/v0.0.1/clawmast' | head -1 || true)
  [[ -n "${NEW_WORKER}" && "${NEW_WORKER}" != "${WORKER_PID}" ]] && break
  sleep 0.1
  NEW_WORKER=""
done
[[ -n "${NEW_WORKER}" ]] || { echo "FAIL: no respawn within 5 s"; tail -40 "${PREFIX}/logs/clawmastd.log"; exit 1; }
echo "[drill] respawn ${WORKER_PID} -> ${NEW_WORKER}"

# Step 5 — HTTP accepts traffic again through the respawned worker.
for i in $(seq 1 50); do
  curl -sf "http://127.0.0.1:${SMOKE_PORT}/api/health" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -sf "http://127.0.0.1:${SMOKE_PORT}/api/openclaw/status" > "${PREFIX}/logs/oc-status2.json" \
  || { echo "FAIL: /api/openclaw/status after respawn"; exit 1; }

# Step 6 — graceful SIGTERM and history events.
kill -TERM "${SUP}"
wait "${SUP}" 2>/dev/null
EXIT=$?
SUP=""
[[ "${EXIT}" -eq 0 ]] || { echo "FAIL: clawmastd exit ${EXIT}"; tail -40 "${PREFIX}/logs/clawmastd.log"; exit 1; }

HIST="${PREFIX}/state/history.json"
SPAWN=$(grep -c '"event":"spawn"' "${HIST}" || true)
CRASH=$(grep -c '"event":"crash"' "${HIST}" || true)
STOP=$(grep -c '"event":"stop"'  "${HIST}" || true)
echo "[drill] history: spawn=${SPAWN} crash=${CRASH} stop=${STOP}"
[[ "${SPAWN}" -ge 2 ]] || { echo "FAIL: want >=2 spawn (initial + respawn)"; exit 1; }
[[ "${CRASH}" -ge 1 ]] || { echo "FAIL: want >=1 crash from SIGKILL";        exit 1; }
[[ "${STOP}"  -ge 1 ]] || { echo "FAIL: want >=1 stop from SIGTERM";          exit 1; }

echo "[drill] PASS"
