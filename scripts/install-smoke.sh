#!/usr/bin/env bash
# Smoke test for scripts/install.sh.
#
# Exercises the installer against a throwaway prefix with --no-service
# so it is safe to run in CI and on developer machines without
# touching launchd / systemd. Verifies the on-disk layout contract
# from architecture/refactor.md §5 and checks that re-running the
# installer performs a correct current → previous rotation when the
# version label changes.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Short path under /tmp: macOS caps AF_UNIX sun_path at 104 bytes, and
# future tests in this script may exercise the supervisor's notify
# socket against this tree.
PREFIX="$(mktemp -d /tmp/cmins.XXXX)"
trap 'rm -rf "${PREFIX}"' EXIT

cd "${REPO_ROOT}"
echo "[smoke] building binaries"
make build >/dev/null

echo "[smoke] fresh install at ${PREFIX}"
bash "${SCRIPT_DIR}/install.sh" --prefix "${PREFIX}" --version v0.0.1 --source local --no-service

assert_file()    { [[ -f "$1" ]] || { echo "FAIL: missing file $1"; exit 1; }; }
assert_dir()     { [[ -d "$1" ]] || { echo "FAIL: missing dir $1";  exit 1; }; }
assert_symlink() { [[ -L "$1" ]] || { echo "FAIL: missing symlink $1"; exit 1; }; }
assert_target()  {
  local link="$1" want="$2"
  local got; got="$(readlink "${link}")"
  [[ "${got}" == "${want}" ]] || { echo "FAIL: ${link} -> ${got}, want ${want}"; exit 1; }
}

echo "[smoke] asserting layout"
assert_dir     "${PREFIX}/bin"
assert_file    "${PREFIX}/bin/clawmastd"
assert_dir     "${PREFIX}/versions/v0.0.1"
assert_file    "${PREFIX}/versions/v0.0.1/clawmast"
assert_file    "${PREFIX}/versions/v0.0.1/MANIFEST.json"
assert_symlink "${PREFIX}/current"
assert_target  "${PREFIX}/current" "versions/v0.0.1"
assert_file    "${PREFIX}/state/channel"
assert_file    "${PREFIX}/keys/minisign.pub"
assert_dir     "${PREFIX}/run"
assert_dir     "${PREFIX}/logs"
assert_dir     "${PREFIX}/data"

# After a fresh install there is no previous (nothing to roll back to).
if [[ -L "${PREFIX}/previous" ]]; then
  echo "FAIL: previous should not exist after a fresh install"
  exit 1
fi

# Binary must run through the current symlink.
VERSION_OUT="$("${PREFIX}/current/clawmast" version)"
echo "[smoke] worker reports: ${VERSION_OUT}"
[[ -n "${VERSION_OUT}" ]] || { echo "FAIL: empty worker version output"; exit 1; }
[[ "${VERSION_OUT}" == *"go1."* ]] || { echo "FAIL: worker version missing go toolchain marker"; exit 1; }

echo "[smoke] upgrade to v0.0.2 → previous must rotate"
bash "${SCRIPT_DIR}/install.sh" --prefix "${PREFIX}" --version v0.0.2 --source local --no-service

assert_symlink "${PREFIX}/current"
assert_symlink "${PREFIX}/previous"
assert_target  "${PREFIX}/current"  "versions/v0.0.2"
assert_target  "${PREFIX}/previous" "versions/v0.0.1"
assert_file    "${PREFIX}/versions/v0.0.2/clawmast"
assert_file    "${PREFIX}/versions/v0.0.2/MANIFEST.json"

# Idempotency: re-running with the same label should not rotate.
echo "[smoke] re-run v0.0.2 → no rotation"
bash "${SCRIPT_DIR}/install.sh" --prefix "${PREFIX}" --version v0.0.2 --source local --no-service
assert_target  "${PREFIX}/current"  "versions/v0.0.2"
assert_target  "${PREFIX}/previous" "versions/v0.0.1"

echo "[smoke] end-to-end: clawmastd boots against install root"
# Use a dedicated port so parallel runs and the developer's own worker
# on the default 17080 never collide.
SMOKE_PORT=17091
CLAWMAST_HTTP_ADDR="127.0.0.1:${SMOKE_PORT}" \
  "${PREFIX}/bin/clawmastd" -install-root "${PREFIX}" \
  > "${PREFIX}/logs/smoke.log" 2>&1 &
SUP=$!
for i in $(seq 1 50); do
  grep -q "worker ready" "${PREFIX}/logs/smoke.log" 2>/dev/null && break
  sleep 0.1
done

# Probe the embedded UI + JSON API while the worker is up
# (refactor.md §9: "UI shows version, check for updates button").
echo "[smoke] probing worker HTTP surface on :${SMOKE_PORT}"
curl -sf "http://127.0.0.1:${SMOKE_PORT}/"             > "${PREFIX}/logs/index.html" \
  || { echo "FAIL: GET / did not return 200"; cat "${PREFIX}/logs/smoke.log"; exit 1; }
curl -sf "http://127.0.0.1:${SMOKE_PORT}/api/health"   > "${PREFIX}/logs/health.json" \
  || { echo "FAIL: /api/health did not return 200";   exit 1; }
curl -sf "http://127.0.0.1:${SMOKE_PORT}/api/version"  > "${PREFIX}/logs/version.json" \
  || { echo "FAIL: /api/version did not return 200";  exit 1; }
curl -sf -X POST "http://127.0.0.1:${SMOKE_PORT}/api/updates/check" \
  > "${PREFIX}/logs/updates.json" \
  || { echo "FAIL: /api/updates/check did not return 200"; exit 1; }
curl -sf "http://127.0.0.1:${SMOKE_PORT}/api/history?limit=10" \
  > "${PREFIX}/logs/history.json" \
  || { echo "FAIL: /api/history did not return 200"; exit 1; }

grep -q '<title>ClawMast</title>'   "${PREFIX}/logs/index.html"   || { echo "FAIL: index.html missing title"; exit 1; }
grep -q '"ok":true'                 "${PREFIX}/logs/health.json"  || { echo "FAIL: health.json missing ok=true"; exit 1; }
# /api/version reflects internal/version.Version (ldflags), not the
# install-root symlink label. Just verify the payload is well-formed.
grep -q '"version":'                "${PREFIX}/logs/version.json" || { echo "FAIL: version.json missing version field"; exit 1; }
grep -q '"go_version":"go1'         "${PREFIX}/logs/version.json" || { echo "FAIL: version.json missing go_version"; exit 1; }
grep -q '"update_available":false'  "${PREFIX}/logs/updates.json" || { echo "FAIL: updates.json missing update_available field"; exit 1; }
# history.json served over HTTP mirrors state/history.json on disk.
grep -q '"event":"spawn"'           "${PREFIX}/logs/history.json" || { echo "FAIL: /api/history missing spawn event"; exit 1; }
grep -q '"path":'                   "${PREFIX}/logs/history.json" || { echo "FAIL: /api/history missing path field"; exit 1; }

kill -TERM "${SUP}"
wait "${SUP}"
EXIT=$?
[[ "${EXIT}" -eq 0 ]] || { echo "FAIL: clawmastd exited ${EXIT}"; cat "${PREFIX}/logs/smoke.log"; exit 1; }
grep -q '"event":"spawn"'       "${PREFIX}/state/history.json" || { echo "FAIL: no spawn in history"; exit 1; }
grep -q '"event":"stop"'        "${PREFIX}/state/history.json" || { echo "FAIL: no stop in history";  exit 1; }

echo "[smoke] PASS"
