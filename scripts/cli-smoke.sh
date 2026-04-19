#!/usr/bin/env bash
# Smoke test for the user-facing `clawmast` subcommands (status, doctor,
# logs). Exercises them against a throwaway install tree so it is safe
# to run in CI and on developer machines without touching launchd /
# systemd or any real install root. Covers three scenarios:
#
#   1. tree exists + daemon up        → status prints supervisor +
#                                       uptime; doctor exits 0; logs
#                                       prints both log files.
#   2. tree exists + daemon down      → status prints "unreachable";
#                                       doctor still exits 0 (checks
#                                       are informational).
#   3. tree does not exist            → doctor exits 1 with a clear
#                                       install-root FAIL.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PREFIX="$(mktemp -d /tmp/cmcli.XXXX)"
trap 'if [[ -n "${SUP:-}" ]]; then kill "${SUP}" 2>/dev/null || true; fi; rm -rf "${PREFIX}"' EXIT

cd "${REPO_ROOT}"
echo "[cli-smoke] building binaries"
make build >/dev/null

echo "[cli-smoke] install at ${PREFIX} (no service, no symlinks)"
bash "${SCRIPT_DIR}/install.sh" \
  --prefix "${PREFIX}" --version v0.0.1 --source local --no-service

CLI="${REPO_ROOT}/bin/clawmast"
SMOKE_PORT=17092
URL="http://127.0.0.1:${SMOKE_PORT}"

# Common env for every CLI invocation: point at the throwaway tree and
# at the port clawmastd will listen on. Exported inline so CLI commands
# pick them up without touching the developer's shell.
run_cli() {
  CLAWMAST_HOME="${PREFIX}" CLAWMAST_HTTP_ADDR="127.0.0.1:${SMOKE_PORT}" \
    "${CLI}" "$@"
}

# Scenario 1: daemon up --------------------------------------------------
# Redirect clawmastd into the same file paths launchd / systemd would
# so `clawmast logs` has something to tail. The worker logs via slog to
# stderr, which the real plist funnels into clawmastd.err.log, so we
# mirror that mapping here.
echo "[cli-smoke] booting clawmastd on :${SMOKE_PORT}"
OUT_LOG="${PREFIX}/logs/clawmastd.out.log"
ERR_LOG="${PREFIX}/logs/clawmastd.err.log"
CLAWMAST_HTTP_ADDR="127.0.0.1:${SMOKE_PORT}" \
  "${PREFIX}/bin/clawmastd" -install-root "${PREFIX}" \
  >"${OUT_LOG}" 2>"${ERR_LOG}" &
SUP=$!
for _ in $(seq 1 50); do
  grep -q "worker ready" "${ERR_LOG}" 2>/dev/null && break
  sleep 0.1
done
grep -q "worker ready" "${ERR_LOG}" \
  || { echo "FAIL: clawmastd did not reach worker ready"; cat "${ERR_LOG}"; exit 1; }

echo "[cli-smoke] status (daemon up)"
STATUS_OUT="$(run_cli status)"
echo "${STATUS_OUT}"
grep -q "install root : ${PREFIX}"   <<<"${STATUS_OUT}" || { echo "FAIL: status missing install root"; exit 1; }
grep -q "daemon       : running"     <<<"${STATUS_OUT}" || { echo "FAIL: status did not detect running daemon"; exit 1; }
grep -q "supervisor   : "            <<<"${STATUS_OUT}" || { echo "FAIL: status missing supervisor line"; exit 1; }
grep -q "channel      : stable"      <<<"${STATUS_OUT}" || { echo "FAIL: status missing channel=stable"; exit 1; }
grep -q "bearer token : "            <<<"${STATUS_OUT}" || { echo "FAIL: status missing bearer token line"; exit 1; }

echo "[cli-smoke] doctor (daemon up)"
DOCTOR_OUT="$(run_cli doctor)"
echo "${DOCTOR_OUT}"
# clawmast-on-PATH is intentionally excluded from the OK list: the
# installer skipped symlinks for this throwaway tree, so it is allowed
# to WARN on both CI and developer machines that never ran make install.
for line in \
  "\[OK  \] install root" \
  "\[OK  \] bin/" \
  "\[OK  \] versions/" \
  "\[OK  \] state/" \
  "\[OK  \] logs/" \
  "\[OK  \] clawmastd version" \
  "\[OK  \] clawmast version" \
  "\[OK  \] bearer token" \
  "\[OK  \] daemon http" \
  "\[OK  \] port ${SMOKE_PORT}" \
  "all critical checks passed"
do
  grep -qE "${line}" <<<"${DOCTOR_OUT}" || { echo "FAIL: doctor output missing line matching: ${line}"; exit 1; }
done

echo "[cli-smoke] logs -n 5 (daemon up)"
LOGS_OUT="$(run_cli logs -n 5)"
echo "${LOGS_OUT}"
grep -q "clawmastd.err.log" <<<"${LOGS_OUT}" || { echo "FAIL: logs did not print err log header"; exit 1; }
grep -q "http server listening" <<<"${LOGS_OUT}" || { echo "FAIL: logs did not contain expected slog line"; exit 1; }

# Scenario 2: daemon down -----------------------------------------------
echo "[cli-smoke] stopping clawmastd"
kill -TERM "${SUP}"
wait "${SUP}" || true
SUP=""
sleep 0.3

echo "[cli-smoke] status (daemon down)"
STATUS_DOWN="$(run_cli status)"
echo "${STATUS_DOWN}"
grep -q "daemon       : unreachable" <<<"${STATUS_DOWN}" || { echo "FAIL: status should mark daemon unreachable after shutdown"; exit 1; }

echo "[cli-smoke] doctor (daemon down, exit code still 0)"
if run_cli doctor >"${PREFIX}/logs/doctor-down.out" 2>&1; then
  grep -q "WARN" "${PREFIX}/logs/doctor-down.out" || { echo "FAIL: doctor should WARN when daemon is down"; exit 1; }
else
  echo "FAIL: doctor should not FAIL when only the daemon is down (exit=$?)"
  cat "${PREFIX}/logs/doctor-down.out"
  exit 1
fi

# Scenario 3: install tree missing --------------------------------------
echo "[cli-smoke] doctor against nonexistent tree must exit 1"
set +e
CLAWMAST_HOME="/tmp/cm-does-not-exist.$$" CLAWMAST_HTTP_ADDR="127.0.0.1:${SMOKE_PORT}" \
  "${CLI}" doctor >"${PREFIX}/logs/doctor-missing.out" 2>&1
RC=$?
set -e
[[ "${RC}" -eq 1 ]] || { echo "FAIL: doctor against missing tree should exit 1, got ${RC}"; cat "${PREFIX}/logs/doctor-missing.out"; exit 1; }
grep -q "\[FAIL\] install root" "${PREFIX}/logs/doctor-missing.out" \
  || { echo "FAIL: doctor output missing [FAIL] install root line"; cat "${PREFIX}/logs/doctor-missing.out"; exit 1; }

echo "[cli-smoke] PASS"
