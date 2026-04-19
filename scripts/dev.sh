#!/usr/bin/env bash
# scripts/dev.sh — UI iteration mode.
#
# Stops the installed launchd/systemd daemon, runs the worker in the
# foreground with CLAWMAST_DEV_DIST_DIR pointed at the checked-in
# internal/embed/dist/ tree, and restores the daemon on exit. Browser
# refresh picks up any edit to HTML/CSS/JS without a rebuild.
#
# Go code changes still require Ctrl+C + re-run (the worker is
# compiled fresh every time this script starts).
#
# Reuses the installed state dir so the bearer token in the browser
# stays valid, and binds to the same port (17080) so the same tab
# keeps working.
set -Eeuo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
dist_dir="$repo_root/internal/embed/dist"
install_root="${CLAWMAST_HOME:-${CLAWMAST_INSTALL_ROOT:-$HOME/.local/share/clawmast}}"
http_addr="${CLAWMAST_HTTP_ADDR:-127.0.0.1:17080}"

if [[ ! -d $dist_dir ]]; then
  echo "[dev] dist dir not found: $dist_dir" >&2
  exit 1
fi
if [[ ! -d $install_root/state ]]; then
  echo "[dev] install root missing state/: $install_root" >&2
  echo "[dev] run 'make install' first so the worker can load the bearer token." >&2
  exit 1
fi

os=$(uname -s)
plist="$HOME/Library/LaunchAgents/com.clawmast.clawmastd.plist"
systemd_unit="com.clawmast.clawmastd.service"

stop_daemon() {
  case $os in
    Darwin)
      if [[ -f $plist ]] && launchctl list | grep -q com.clawmast.clawmastd; then
        echo "[dev] launchctl unload $plist"
        launchctl unload "$plist" 2>/dev/null || true
        daemon_was_running=1
      fi ;;
    Linux)
      if systemctl --user is-active --quiet "$systemd_unit" 2>/dev/null; then
        echo "[dev] systemctl --user stop $systemd_unit"
        systemctl --user stop "$systemd_unit" || true
        daemon_was_running=1
      fi ;;
  esac
}

restore_daemon() {
  [[ -z ${daemon_was_running:-} ]] && return
  case $os in
    Darwin)
      echo "[dev] launchctl load $plist"
      launchctl load "$plist" 2>/dev/null || true ;;
    Linux)
      echo "[dev] systemctl --user start $systemd_unit"
      systemctl --user start "$systemd_unit" || true ;;
  esac
}

dev_bin=""
cleanup() {
  echo
  echo "[dev] shutting down worker, restoring installed daemon…"
  restore_daemon
  [[ -n $dev_bin && -f $dev_bin ]] && rm -f "$dev_bin"
}
trap cleanup EXIT INT TERM

stop_daemon

# Wait for the old listener to release the port. launchctl unload is
# async; binding too early races with the outgoing supervisor. Give
# it up to 10s; if the port is still held we surface the conflict
# rather than pretend the unload worked.
port="${http_addr##*:}"
for i in $(seq 1 40); do
  if ! lsof -i TCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    break
  fi
  if (( i == 40 )); then
    echo "[dev] :$port still held after 10s — aborting. Try: launchctl list | grep clawmast" >&2
    exit 1
  fi
  sleep 0.25
done

cat <<BANNER
────────────────────────────────────────────────────────────────
 clawmast dev mode
   UI source     $dist_dir
   install root  $install_root
   http addr     $http_addr
   token path    $install_root/state/token

 Edit HTML / CSS / JS under internal/embed/dist/ — then just
 hit Cmd+Shift+R in the browser. No rebuild. No install.

 Ctrl+C to quit and restore the installed daemon.
────────────────────────────────────────────────────────────────
BANNER

export CLAWMAST_DEV_DIST_DIR="$dist_dir"
export CLAWMAST_INSTALL_ROOT="$install_root"
export CLAWMAST_HTTP_ADDR="$http_addr"
# Disable sdnotify — we're running unsupervised, so the worker
# shouldn't try to hand-shake with a non-existent clawmastd.
unset NOTIFY_SOCKET

cd "$repo_root"
# Compile to a real file rather than `go run` — `go run`'s signal
# forwarding to the compiled child is unreliable across Go versions
# and can leave orphaned workers holding the port, breaking the
# next `make dev` invocation. A plain binary inherits signals cleanly
# and makes the EXIT trap's tear-down deterministic.
dev_bin="$(mktemp -t clawmast-dev)"
echo "[dev] building → $dev_bin"
go build -o "$dev_bin" ./cmd/clawmast

"$dev_bin" &
worker_pid=$!
forward() { kill -"$1" "$worker_pid" 2>/dev/null || true; }
trap 'forward TERM' TERM
trap 'forward INT'  INT
wait "$worker_pid" 2>/dev/null || true
