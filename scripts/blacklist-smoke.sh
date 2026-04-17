#!/usr/bin/env bash
# T1-03 — version blacklist smoke test.
#
# Installs two good versions of the worker (v0.0.1 as previous, v0.0.2
# as current), boots clawmastd, then hits POST /api/blacklist on the
# running v0.0.2 worker. This exercises the full blacklist flow:
#
#   1. Worker writes state/blacklist.json with the current version.
#   2. Worker exits with code 65 (ClassRollback per protocol §4).
#   3. Supervisor performs the current↔previous symlink swap.
#   4. Supervisor spawns v0.0.1 and reaches Running.
#   5. (Anti-regression) If somehow current still pointed at a
#      blacklisted version on a subsequent boot, supervisor.checkBlacklist
#      would catch it pre-spawn — tested indirectly by the end state.
#
# Covers architecture/refactor.md §9 (Iteration 1: version blacklist)
# and the class-Rollback path in supervisor-protocol.md §4.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Short prefix: AF_UNIX paths on macOS cap at 104 bytes.
PREFIX="$(mktemp -d /tmp/cmbl.XXXX)"
PORT=17183
SUP=""
trap 'cleanup' EXIT
cleanup() {
  if [[ -n "${SUP}" ]] && kill -0 "${SUP}" 2>/dev/null; then
    kill -TERM "${SUP}" 2>/dev/null || true
    wait "${SUP}" 2>/dev/null || true
  fi
  rm -rf "${PREFIX}"
}

cd "${REPO_ROOT}"

echo "[bl] building binaries"
make build >/dev/null

echo "[bl] installing v0.0.1 then v0.0.2 (so current=v0.0.2, previous=v0.0.1)"
bash "${SCRIPT_DIR}/install.sh" --prefix "${PREFIX}" --version v0.0.1 --source local --no-service >/dev/null
bash "${SCRIPT_DIR}/install.sh" --prefix "${PREFIX}" --version v0.0.2 --source local --no-service >/dev/null

CUR_TGT="$(readlink "${PREFIX}/current")"
[[ "${CUR_TGT}" == "versions/v0.0.2" ]] || { echo "FAIL: current=${CUR_TGT}, want versions/v0.0.2"; exit 1; }

echo "[bl] booting clawmastd on port ${PORT}"
mkdir -p "${PREFIX}/logs"
CLAWMAST_HTTP_ADDR="127.0.0.1:${PORT}" \
  "${PREFIX}/bin/clawmastd" -install-root "${PREFIX}" -log-level debug \
  > "${PREFIX}/logs/clawmastd.log" 2>&1 &
SUP=$!

echo "[bl] waiting for worker HTTP server to come up"
for i in $(seq 1 100); do
  if curl -sf "http://127.0.0.1:${PORT}/api/version" > "${PREFIX}/version.json" 2>/dev/null; then
    break
  fi
  sleep 0.1
  if [[ "${i}" == "100" ]]; then
    echo "FAIL: /api/version never responded"
    tail -50 "${PREFIX}/logs/clawmastd.log"
    exit 1
  fi
done
# The ldflags-injected "version" may be a git-describe fallback on
# local dev builds; the supervisor-assigned "label" is what we care
# about (it matches versions/<LABEL>/ on disk and is what the
# blacklist keys on).
WORKER_LABEL="$(grep -o '"label":"[^"]*"' "${PREFIX}/version.json" | head -1 | cut -d'"' -f4)"
echo "[bl] worker running label=${WORKER_LABEL}"
[[ "${WORKER_LABEL}" == "v0.0.2" ]] || { echo "FAIL: expected label v0.0.2, got ${WORKER_LABEL}"; exit 1; }

echo "[bl] POST /api/blacklist to mark v0.0.2 as bad"
HTTP_CODE="$(curl -s -o "${PREFIX}/blacklist.json" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' \
  -d '{"reason":"smoke-test"}' \
  "http://127.0.0.1:${PORT}/api/blacklist")"
[[ "${HTTP_CODE}" == "202" ]] || { echo "FAIL: POST returned ${HTTP_CODE}"; cat "${PREFIX}/blacklist.json"; exit 1; }
grep -q '"marked_version":"v0.0.2"'   "${PREFIX}/blacklist.json" || { echo "FAIL: marked_version missing"; cat "${PREFIX}/blacklist.json"; exit 1; }
grep -q '"rollback_requested":true'   "${PREFIX}/blacklist.json" || { echo "FAIL: rollback_requested missing"; cat "${PREFIX}/blacklist.json"; exit 1; }

echo "[bl] waiting for blacklist.json to contain v0.0.2"
# blacklist.json is MarshalIndent-formatted (one field per line), so
# match tolerates a space between the colon and the value.
BL_PAT='"version":[[:space:]]*"v0\.0\.2"'
for i in $(seq 1 50); do
  if [[ -f "${PREFIX}/state/blacklist.json" ]] \
     && grep -qE "${BL_PAT}" "${PREFIX}/state/blacklist.json"; then break; fi
  sleep 0.1
done
grep -qE "${BL_PAT}" "${PREFIX}/state/blacklist.json" \
  || { echo "FAIL: blacklist.json missing v0.0.2"; cat "${PREFIX}/state/blacklist.json" 2>/dev/null; exit 1; }

echo "[bl] waiting for supervisor to roll back (up to 15 s)"
for i in $(seq 1 150); do
  if [[ -f "${PREFIX}/state/history.json" ]] \
     && grep -q '"event":"rollback".*"version":"v0.0.2"' "${PREFIX}/state/history.json"; then break; fi
  sleep 0.1
  if [[ "${i}" == "150" ]]; then
    echo "FAIL: no rollback event after 15 s"
    echo "--- history ---"; cat "${PREFIX}/state/history.json" 2>/dev/null || echo "(none)"
    echo "--- log ---";     tail -60 "${PREFIX}/logs/clawmastd.log"
    exit 1
  fi
done

echo "[bl] waiting for v0.0.1 to come back up"
for i in $(seq 1 100); do
  if curl -sf "http://127.0.0.1:${PORT}/api/version" > "${PREFIX}/version2.json" 2>/dev/null; then
    NEW_LABEL="$(grep -o '"label":"[^"]*"' "${PREFIX}/version2.json" | head -1 | cut -d'"' -f4)"
    [[ "${NEW_LABEL}" == "v0.0.1" ]] && break
  fi
  sleep 0.1
done

echo "[bl] symlinks after rollback:"
ls -la "${PREFIX}/current" "${PREFIX}/previous"
CUR_TGT="$(readlink "${PREFIX}/current")"
PREV_TGT="$(readlink "${PREFIX}/previous")"
[[ "${CUR_TGT}"  == "versions/v0.0.1" ]] || { echo "FAIL: current=${CUR_TGT}, want versions/v0.0.1"; exit 1; }
[[ "${PREV_TGT}" == "versions/v0.0.2" ]] || { echo "FAIL: previous=${PREV_TGT}, want versions/v0.0.2"; exit 1; }

# History assertions: rollback event must be reason="rollback-requested"
# to distinguish it from crash-loop-driven rollbacks.
grep -q '"event":"rollback".*"version":"v0.0.2".*"reason":"rollback-requested"' "${PREFIX}/state/history.json" \
  || { echo "FAIL: rollback event must carry reason=rollback-requested"; tail -10 "${PREFIX}/state/history.json"; exit 1; }

# Worker exit 65 must be recorded as a crash with exit_code=65 and
# reason=rollback (per classifyOutcome).
grep -q '"event":"crash".*"version":"v0.0.2".*"exit_code":65' "${PREFIX}/state/history.json" \
  || { echo "FAIL: no crash/exit=65 event for bad version"; tail -10 "${PREFIX}/state/history.json"; exit 1; }

echo "[bl] graceful shutdown"
kill -TERM "${SUP}"
wait "${SUP}"
EXIT=$?
SUP=""
[[ "${EXIT}" -eq 0 ]] || { echo "FAIL: clawmastd exited ${EXIT}"; tail -30 "${PREFIX}/logs/clawmastd.log"; exit 1; }

echo "[bl] history tail:"
tail -10 "${PREFIX}/state/history.json"
echo "[bl] blacklist:"
cat "${PREFIX}/state/blacklist.json"
echo "[bl] PASS"
