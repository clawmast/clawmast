#!/usr/bin/env bash
# ClawMast auto-update playground.
#
# Persistent local update environment you can click through in the
# browser. Unlike scripts/update-install-smoke.sh (which tears itself
# down in a few seconds), this script leaves the supervisor + channel
# HTTP server running so you can:
#
#   - open http://127.0.0.1:17080/ in a real browser
#   - click "检查更新", watch it succeed
#   - publish a new version from another terminal
#   - click "下载并安装", watch the supervisor respawn
#   - toggle stable/beta and watch the worker restart
#
# Usage:
#   scripts/playground.sh up       start supervisor + channel server
#   scripts/playground.sh publish  build a new worker, sign a manifest,
#                                  drop it into the channel dir
#                                  (V=vX.Y.Z to override the label)
#   scripts/playground.sh status   show running PIDs, URL, tail logs
#   scripts/playground.sh down     kill processes, keep the install tree
#   scripts/playground.sh nuke     down + rm -rf the install tree

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PLAY_ROOT="${PLAYGROUND_ROOT:-/tmp/clawmast-playground}"
STAGING="${PLAY_ROOT}/_staging"
TMP="${PLAY_ROOT}/_tmp"
CHAN_ROOT="${PLAY_ROOT}/_channel"
LOGS="${PLAY_ROOT}/logs"
PIDFILE_SUP="${PLAY_ROOT}/.sup.pid"
PIDFILE_HTTP="${PLAY_ROOT}/.http.pid"
PUB_KEY="${PLAY_ROOT}/_tmp/channel.pub"
SECRET_KEY="${PLAY_ROOT}/_tmp/channel.key"

WORKER_PORT="${WORKER_PORT:-17080}"
CHAN_PORT="${CHAN_PORT:-17090}"
START_VERSION="${START_VERSION:-v0.1.0-playground}"

msg()  { printf '\033[1;34m[playground]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[playground]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[playground]\033[0m %s\n' "$*" >&2; exit 1; }

ensure_free_port() {
  local port="$1"
  if lsof -ti:"${port}" >/dev/null 2>&1; then
    die "port ${port} is already bound; free it or override WORKER_PORT / CHAN_PORT"
  fi
}

build_worker() {
  local label="$1" out="$2"
  msg "building worker labelled ${label}"
  go build \
    -ldflags "-s -w \
      -X github.com/clawmast/clawmast/internal/version.Version=${label} \
      -X github.com/clawmast/clawmast/internal/version.Commit=playground \
      -X github.com/clawmast/clawmast/internal/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o "${out}" ./cmd/clawmast
}

cmd_up() {
  if [[ -f "${PIDFILE_SUP}" ]] && kill -0 "$(cat "${PIDFILE_SUP}")" 2>/dev/null; then
    die "playground is already up (pid $(cat "${PIDFILE_SUP}")); run 'down' first"
  fi
  ensure_free_port "${WORKER_PORT}"
  ensure_free_port "${CHAN_PORT}"

  mkdir -p "${STAGING}" "${TMP}" "${CHAN_ROOT}" "${LOGS}"
  cd "${REPO_ROOT}"

  msg "building supervisor + release tool"
  # Supervisor carries its own ldflags so the System card in the UI
  # reports a recognisable value instead of "dev". Using the worker's
  # version label keeps the two lines in sync during a fresh playground.
  go build \
    -ldflags "-s -w \
      -X github.com/clawmast/clawmast/internal/version.Version=${START_VERSION}-d \
      -X github.com/clawmast/clawmast/internal/version.Commit=playground \
      -X github.com/clawmast/clawmast/internal/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o "${STAGING}/clawmastd" ./cmd/clawmastd
  go build -o "${TMP}/clawmast-release" ./cmd/clawmast-release
  build_worker "${START_VERSION}" "${STAGING}/clawmast"

  msg "laying down ${START_VERSION} as current"
  bash "${SCRIPT_DIR}/install.sh" \
    --prefix "${PLAY_ROOT}" \
    --version "${START_VERSION}" \
    --bin-dir "${STAGING}" \
    --no-service \
    >"${LOGS}/install.log" 2>&1

  if [[ ! -f "${PUB_KEY}" ]]; then
    msg "generating throwaway minisign keypair"
    "${TMP}/clawmast-release" keygen -pub "${PUB_KEY}" > "${SECRET_KEY}"
    chmod 600 "${SECRET_KEY}"
  else
    msg "reusing existing keypair at ${PUB_KEY}"
  fi

  msg "starting channel HTTP server on :${CHAN_PORT}"
  (cd "${CHAN_ROOT}" && exec python3 -m http.server "${CHAN_PORT}") \
    >"${LOGS}/channel.log" 2>&1 &
  echo $! > "${PIDFILE_HTTP}"

  msg "starting clawmastd on :${WORKER_PORT}"
  CLAWMAST_HTTP_ADDR="127.0.0.1:${WORKER_PORT}" \
  CLAWMAST_UPDATE_URL="http://127.0.0.1:${CHAN_PORT}" \
  CLAWMAST_UPDATE_CHANNEL="stable" \
  CLAWMAST_UPDATE_PUBKEY_FILE="${PUB_KEY}" \
    "${PLAY_ROOT}/bin/clawmastd" -install-root "${PLAY_ROOT}" \
    >"${LOGS}/clawmastd.log" 2>&1 &
  echo $! > "${PIDFILE_SUP}"

  sleep 0.8
  if ! kill -0 "$(cat "${PIDFILE_SUP}")" 2>/dev/null; then
    warn "supervisor died during boot; last 20 lines:"
    tail -20 "${LOGS}/clawmastd.log" >&2 || true
    rm -f "${PIDFILE_SUP}" "${PIDFILE_HTTP}"
    die "playground failed to start"
  fi

  msg "playground up"
  msg "  root       ${PLAY_ROOT}"
  msg "  worker URL http://127.0.0.1:${WORKER_PORT}/"
  msg "  channel    http://127.0.0.1:${CHAN_PORT}/"
  msg "  version    ${START_VERSION}"
  msg "  token      $(cat "${PLAY_ROOT}/state/token" 2>/dev/null | head -c 16)… (loopback bypass active, paste not required)"
  msg ""
  msg "publish a new version:   scripts/playground.sh publish v0.1.1-playground"
  msg "publish to beta:         scripts/playground.sh publish v0.1.1-beta.1 beta"
  msg "tail logs:               tail -f ${LOGS}/clawmastd.log"
  msg "stop:                    scripts/playground.sh down"
}

cmd_publish() {
  # Usage: publish <version> [channel]
  # Builds a worker labelled <version>, tarballs it, and drops a signed
  # manifest into the channel dir so the next /api/updates/check picks
  # it up. The worker's current channel preference decides whether the
  # UI shows it as an upgrade — a stable-labelled manifest is ignored
  # by a worker running in the beta channel and vice versa.
  local version="${1:-}" channel="${2:-stable}"
  [[ -n "${version}" ]] || die "usage: playground.sh publish <version> [stable|beta]"
  [[ "${channel}" == "stable" || "${channel}" == "beta" ]] \
    || die "channel must be 'stable' or 'beta' (got '${channel}')"
  [[ -f "${SECRET_KEY}" ]] || die "no keypair at ${SECRET_KEY}; run 'up' first"
  [[ -x "${TMP}/clawmast-release" ]] || die "no release tool; run 'up' first"

  mkdir -p "${TMP}/publish"
  local workdir="${TMP}/publish/${version}"
  rm -rf "${workdir}" && mkdir -p "${workdir}"

  cd "${REPO_ROOT}"
  build_worker "${version}" "${workdir}/clawmast"

  local os arch tarball sha size
  os="$(go env GOOS)"
  arch="$(go env GOARCH)"
  tarball="clawmast-${version}-${os}-${arch}.tar.gz"
  (cd "${workdir}" && tar -czf "${CHAN_ROOT}/${tarball}" clawmast)
  sha="$(shasum -a 256 "${CHAN_ROOT}/${tarball}" | awk '{print $1}')"
  size="$(wc -c < "${CHAN_ROOT}/${tarball}" | tr -d ' ')"

  mkdir -p "${CHAN_ROOT}/${channel}"
  cat > "${CHAN_ROOT}/${channel}/manifest.json" <<JSON
{
  "channel": "${channel}",
  "version": "${version}",
  "published_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "notes": "playground publish (${channel})",
  "artifacts": [
    {
      "os": "${os}", "arch": "${arch}",
      "url": "http://127.0.0.1:${CHAN_PORT}/${tarball}",
      "size": ${size},
      "sha256": "${sha}"
    }
  ]
}
JSON
  "${TMP}/clawmast-release" sign \
    -key "${SECRET_KEY}" \
    -in "${CHAN_ROOT}/${channel}/manifest.json" \
    -out "${CHAN_ROOT}/${channel}/manifest.json.minisig"

  msg "published ${version} on channel '${channel}'"
  msg "  manifest  ${CHAN_ROOT}/${channel}/manifest.json"
  msg "  artifact  ${CHAN_ROOT}/${tarball} (${size} bytes, sha256 ${sha:0:12}…)"
  msg ""
  msg "in the UI: open settings → 点「检查更新」→ 应该看到 ${version} 可安装。"
  if [[ "${channel}" == "beta" ]]; then
    msg "注意:当前 worker 若在 stable 通道,会以 channel-mismatch 拒绝这份 manifest。"
    msg "      切到 beta 通道(设置页分段控件)后 worker 会重启,再检查更新即可看到。"
  fi
}

cmd_status() {
  if [[ -f "${PIDFILE_SUP}" ]] && kill -0 "$(cat "${PIDFILE_SUP}")" 2>/dev/null; then
    msg "supervisor: UP (pid $(cat "${PIDFILE_SUP}"))"
  else
    msg "supervisor: DOWN"
  fi
  if [[ -f "${PIDFILE_HTTP}" ]] && kill -0 "$(cat "${PIDFILE_HTTP}")" 2>/dev/null; then
    msg "channel   : UP (pid $(cat "${PIDFILE_HTTP}"))"
  else
    msg "channel   : DOWN"
  fi
  msg "root      : ${PLAY_ROOT}"
  msg "worker URL: http://127.0.0.1:${WORKER_PORT}/"
  msg "channel URL: http://127.0.0.1:${CHAN_PORT}/"
  if [[ -L "${PLAY_ROOT}/current" ]]; then
    msg "current   : $(readlink "${PLAY_ROOT}/current")"
  fi
  if [[ -L "${PLAY_ROOT}/previous" ]]; then
    msg "previous  : $(readlink "${PLAY_ROOT}/previous")"
  fi
  if [[ -f "${PLAY_ROOT}/state/channel" ]]; then
    msg "channel   : $(cat "${PLAY_ROOT}/state/channel") (persisted)"
  fi
}

stop_pidfile() {
  local label="$1" pidfile="$2"
  if [[ ! -f "${pidfile}" ]]; then return 0; fi
  local pid
  pid="$(cat "${pidfile}")"
  if kill -0 "${pid}" 2>/dev/null; then
    msg "stopping ${label} (pid ${pid})"
    kill -TERM "${pid}" 2>/dev/null || true
    # Wait up to 3s for graceful shutdown.
    for _ in 1 2 3 4 5 6; do
      sleep 0.5
      kill -0 "${pid}" 2>/dev/null || break
    done
    kill -0 "${pid}" 2>/dev/null && kill -KILL "${pid}" 2>/dev/null || true
  fi
  rm -f "${pidfile}"
}

cmd_down() {
  stop_pidfile "supervisor" "${PIDFILE_SUP}"
  stop_pidfile "channel-http" "${PIDFILE_HTTP}"
  msg "playground down. install tree kept at ${PLAY_ROOT} (use 'nuke' to wipe)."
}

cmd_nuke() {
  cmd_down
  if [[ -d "${PLAY_ROOT}" ]]; then
    msg "wiping ${PLAY_ROOT}"
    rm -rf "${PLAY_ROOT}"
  fi
}

case "${1:-}" in
  up)      shift; cmd_up "$@" ;;
  publish) shift; cmd_publish "$@" ;;
  status)  shift; cmd_status "$@" ;;
  down)    shift; cmd_down "$@" ;;
  nuke)    shift; cmd_nuke "$@" ;;
  ""|-h|--help|help)
    sed -n '2,24p' "${BASH_SOURCE[0]}"
    ;;
  *)
    die "unknown subcommand '${1}'. try: up | publish | status | down | nuke"
    ;;
esac
