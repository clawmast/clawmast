#!/usr/bin/env bash
# ClawMast uninstaller (Tier 1: macOS and Linux).
#
# Reverses scripts/install.sh: tears down the user-level launchd /
# systemd unit and (unless --keep-data) wipes the install root defined
# in architecture/refactor.md §5.
#
# Idempotent: safe to re-run after a partial uninstall. Honours the
# same CLAWMAST_HOME / --prefix override as install.sh.
#
# Windows is Tier 2, worker-only, manual install — this script refuses
# to run there.

set -euo pipefail

PREFIX="${CLAWMAST_HOME:-${HOME}/.clawmast}"
KEEP_DATA="no"
ASSUME_YES="no"
DRY_RUN="no"

usage() {
  cat <<'EOF'
Usage: uninstall.sh [options]

Options:
  --prefix PATH   Install root to remove (default: $CLAWMAST_HOME or ~/.clawmast)
  --keep-data     Remove binaries and the service unit but preserve
                  state/, data/, logs/, and keys/ under the prefix.
  --dry-run       Print what would be removed without touching anything.
  -y, --yes       Do not prompt before deleting the prefix.
  -h, --help      Show this help and exit.

Environment overrides:
  CLAWMAST_HOME   Default for --prefix.
EOF
}

log()  { printf '\033[1;36m[uninstall]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m      %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[fatal]\033[0m     %s\n' "$*" >&2; exit 1; }

# Argument parsing ------------------------------------------------------------

while (( $# > 0 )); do
  case "$1" in
    --prefix)      PREFIX="$2"; shift 2 ;;
    --keep-data)   KEEP_DATA="yes"; shift ;;
    --dry-run)     DRY_RUN="yes"; shift ;;
    -y|--yes)      ASSUME_YES="yes"; shift ;;
    -h|--help)     usage; exit 0 ;;
    *)             die "unknown option: $1" ;;
  esac
done

# Platform detection ----------------------------------------------------------

UNAME_S="$(uname -s)"
case "${UNAME_S}" in
  Darwin) OS="darwin" ;;
  Linux)  OS="linux"  ;;
  *)      die "unsupported OS: ${UNAME_S} (Windows is Tier 2, manual install)" ;;
esac
log "host: ${OS}"
log "prefix: ${PREFIX}"
[[ "${DRY_RUN}" == "yes" ]] && log "dry-run: no changes will be made"

run() {
  if [[ "${DRY_RUN}" == "yes" ]]; then
    printf '  would run: %s\n' "$*"
  else
    "$@"
  fi
}

# Service teardown ------------------------------------------------------------

remove_launchd() {
  local plist="${HOME}/Library/LaunchAgents/com.clawmast.clawmastd.plist"
  local label="gui/$(id -u)/com.clawmast.clawmastd"

  # Use bootout on Big Sur+; fall back to unload for older installs that
  # were registered with launchctl load.
  if launchctl print "${label}" >/dev/null 2>&1; then
    run launchctl bootout "${label}" 2>/dev/null || \
      run launchctl unload "${plist}" 2>/dev/null || true
    log "launchd unit unloaded"
  else
    log "launchd unit not loaded (already gone)"
  fi

  if [[ -f "${plist}" ]]; then
    run rm -f "${plist}"
    log "removed ${plist}"
  fi
}

remove_systemd_user() {
  local unit="${HOME}/.config/systemd/user/clawmastd.service"
  if command -v systemctl >/dev/null 2>&1; then
    if systemctl --user is-enabled clawmastd.service >/dev/null 2>&1 \
       || systemctl --user is-active clawmastd.service >/dev/null 2>&1; then
      run systemctl --user disable --now clawmastd.service 2>/dev/null || true
      log "systemd user unit disabled + stopped"
    fi
    run systemctl --user daemon-reload 2>/dev/null || true
  else
    warn "systemctl not found; skipping service teardown"
  fi
  if [[ -f "${unit}" ]]; then
    run rm -f "${unit}"
    log "removed ${unit}"
  fi
}

remove_service() {
  case "${OS}" in
    darwin) remove_launchd ;;
    linux)  remove_systemd_user ;;
  esac
}

# Prefix teardown -------------------------------------------------------------

remove_prefix() {
  if [[ ! -e "${PREFIX}" ]]; then
    log "prefix already absent: ${PREFIX}"
    return
  fi

  if [[ "${KEEP_DATA}" == "yes" ]]; then
    log "removing binaries + symlinks; keeping state/ data/ logs/ keys/"
    run rm -rf "${PREFIX}/bin" "${PREFIX}/versions" "${PREFIX}/run" \
               "${PREFIX}/current" "${PREFIX}/previous"
    return
  fi

  if [[ "${ASSUME_YES}" != "yes" && "${DRY_RUN}" != "yes" ]]; then
    printf '\033[1;33mAbout to delete %s (including state, logs, keys).\033[0m\n' "${PREFIX}"
    read -r -p "Type 'yes' to continue: " reply
    [[ "${reply}" == "yes" ]] || die "aborted"
  fi
  run rm -rf "${PREFIX}"
  log "removed ${PREFIX}"
}

main() {
  remove_service
  remove_prefix
  log "uninstall complete"
}

main "$@"
