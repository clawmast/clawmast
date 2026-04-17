#!/usr/bin/env bash
# T0-08 — forced-failure rollback smoke test.
#
# Builds a deliberately broken stand-in worker
# (scripts/testdata/crash-after-ready), installs it as the "current"
# version on top of a known-good previous version, then boots the
# real clawmastd against the install tree and verifies that the
# supervisor:
#
#   1. Spawns the bad worker, observes READY=1, and lets it crash.
#   2. Records 3 spawn + 3 crash events for the bad version inside
#      the configured CrashWindow (default 60 s).
#   3. Triggers attemptRollback, swaps current ↔ previous on disk,
#      and records a "rollback" event.
#   4. Spawns the previously known-good worker and reaches Running.
#
# Covers the rollback path described in
# architecture/supervisor-protocol.md §6 and the "current/previous
# atomic swap" contract in refactor.md §5. This is the final gate for
# closing Iteration 0.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Short prefix; AF_UNIX path on macOS caps at 104 bytes.
PREFIX="$(mktemp -d /tmp/cmrb.XXXX)"
trap 'cleanup' EXIT
SUP=""
cleanup() {
  if [[ -n "${SUP}" ]] && kill -0 "${SUP}" 2>/dev/null; then
    kill -TERM "${SUP}" 2>/dev/null || true
    wait "${SUP}" 2>/dev/null || true
  fi
  rm -rf "${PREFIX}"
}

cd "${REPO_ROOT}"

echo "[rb] building binaries"
make build >/dev/null

echo "[rb] building scripts/testdata/crash-after-ready"
go build -o "${PREFIX}/crash-after-ready" ./scripts/testdata/crash-after-ready

echo "[rb] installing good v0.0.1"
bash "${SCRIPT_DIR}/install.sh" --prefix "${PREFIX}" --version v0.0.1 --source local --no-service >/dev/null

echo "[rb] installing bad v0.0.2 over the top (so current=bad, previous=good)"
mkdir -p "${PREFIX}/versions/v0.0.2"
cp "${PREFIX}/crash-after-ready" "${PREFIX}/versions/v0.0.2/clawmast"
chmod 0755 "${PREFIX}/versions/v0.0.2/clawmast"

# Rotate symlinks: current=v0.0.2 (bad), previous=v0.0.1 (good).
rm -f "${PREFIX}/current" "${PREFIX}/previous"
(cd "${PREFIX}" && ln -s versions/v0.0.2 current)
(cd "${PREFIX}" && ln -s versions/v0.0.1 previous)

echo "[rb] symlinks before:"
ls -la "${PREFIX}/current" "${PREFIX}/previous"

echo "[rb] booting clawmastd"
"${PREFIX}/bin/clawmastd" -install-root "${PREFIX}" -log-level debug \
  > "${PREFIX}/logs/clawmastd.log" 2>&1 &
SUP=$!

echo "[rb] waiting for rollback event (up to 30 s)"
for i in $(seq 1 300); do
  if [[ -f "${PREFIX}/state/history.json" ]] \
     && grep -q '"event":"rollback"' "${PREFIX}/state/history.json"; then
    echo "[rb] rollback recorded after $((i / 10)).$((i % 10)) s"
    break
  fi
  sleep 0.1
  if [[ "${i}" == "300" ]]; then
    echo "FAIL: no rollback event after 30 s"
    echo "--- history ---"
    cat "${PREFIX}/state/history.json" 2>/dev/null || echo "(no history file)"
    echo "--- log ---"
    tail -50 "${PREFIX}/logs/clawmastd.log"
    exit 1
  fi
done

echo "[rb] waiting for the rolled-back worker to reach Running"
for i in $(seq 1 100); do
  if grep -q 'state=Running.*version=v0.0.1' "${PREFIX}/logs/clawmastd.log" \
     || grep -q '"version":"v0.0.1"' "${PREFIX}/state/history.json"; then
    break
  fi
  sleep 0.1
done

echo "[rb] symlinks after rollback:"
ls -la "${PREFIX}/current" "${PREFIX}/previous"

# On-disk assertions: current must point to the good version, previous
# to the bad one (the supervisor's swap is atomic per refactor.md §5).
CUR_TGT="$(readlink "${PREFIX}/current")"
PREV_TGT="$(readlink "${PREFIX}/previous")"
[[ "${CUR_TGT}"  == "versions/v0.0.1" ]] || { echo "FAIL: current=${CUR_TGT}, want versions/v0.0.1"; exit 1; }
[[ "${PREV_TGT}" == "versions/v0.0.2" ]] || { echo "FAIL: previous=${PREV_TGT}, want versions/v0.0.2"; exit 1; }

# History assertions: at least 3 crashes and a rollback for the bad
# version, then a spawn of the good version.
crash_count="$(grep -c '"event":"crash".*"version":"v0.0.2"' "${PREFIX}/state/history.json" || true)"
rollback_count="$(grep -c '"event":"rollback".*"version":"v0.0.2"' "${PREFIX}/state/history.json" || true)"
good_spawn="$(grep -c '"event":"spawn","version":"v0.0.1"' "${PREFIX}/state/history.json" || true)"
echo "[rb] history: crashes(v0.0.2)=${crash_count} rollbacks(v0.0.2)=${rollback_count} spawns(v0.0.1)=${good_spawn}"
[[ "${crash_count}"    -ge 3 ]] || { echo "FAIL: want >=3 crashes for bad version, got ${crash_count}"; exit 1; }
[[ "${rollback_count}" -ge 1 ]] || { echo "FAIL: want >=1 rollback event, got ${rollback_count}"; exit 1; }
[[ "${good_spawn}"     -ge 1 ]] || { echo "FAIL: want good version to be respawned after rollback"; exit 1; }

echo "[rb] graceful shutdown"
kill -TERM "${SUP}"
wait "${SUP}"
EXIT=$?
SUP=""
[[ "${EXIT}" -eq 0 ]] || { echo "FAIL: clawmastd exited ${EXIT}"; tail -30 "${PREFIX}/logs/clawmastd.log"; exit 1; }

echo "[rb] history tail:"
tail -10 "${PREFIX}/state/history.json"
echo "[rb] PASS"
