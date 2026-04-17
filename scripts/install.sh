#!/usr/bin/env bash
# ClawMast installer (Tier 1: macOS and Linux).
#
# Lays out the install root defined in architecture/refactor.md §5,
# drops in the clawmastd supervisor and a versioned clawmast worker,
# flips the `current` symlink atomically, and (unless --no-service)
# registers the appropriate user-level service so clawmastd starts at
# login.
#
# Idempotent: re-running upgrades in place. This is also the official
# escape hatch when auto-update is broken (client-topology.md §4).
#
# Windows is Tier 2, worker-only, manual install (AGENTS.md R5 /
# client-topology.md §2) — this script refuses to run there.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
TEMPLATES_DIR="${SCRIPT_DIR}/templates"

PREFIX="${CLAWMAST_HOME:-${HOME}/.clawmast}"
VERSION_LABEL=""
SOURCE_MODE="auto"      # auto | local | build
INSTALL_SERVICE="yes"
FORCE="no"
CHANNEL_DEFAULT="stable"

usage() {
  cat <<'EOF'
Usage: install.sh [options]

Options:
  --prefix PATH           Install root (default: $CLAWMAST_HOME or ~/.clawmast)
  --version LABEL         Version directory name under versions/ (default: derived)
  --source {auto|local|build}
                          auto  — prefer ./bin, fall back to `go build` (default)
                          local — require pre-built binaries in ./bin
                          build — always rebuild from this repo
  --no-service            Skip writing / enabling the launchd / systemd unit
  --force                 Overwrite existing install without prompting
  -h, --help              Show this help and exit

Environment overrides:
  CLAWMAST_HOME           Default for --prefix
EOF
}

log()  { printf '\033[1;36m[install]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m    %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[fatal]\033[0m   %s\n' "$*" >&2; exit 1; }

# Argument parsing ------------------------------------------------------------

while (( $# > 0 )); do
  case "$1" in
    --prefix)     PREFIX="$2"; shift 2 ;;
    --version)    VERSION_LABEL="$2"; shift 2 ;;
    --source)     SOURCE_MODE="$2"; shift 2 ;;
    --no-service) INSTALL_SERVICE="no"; shift ;;
    --force)      FORCE="yes"; shift ;;
    -h|--help)    usage; exit 0 ;;
    *)            die "unknown option: $1" ;;
  esac
done

# Platform detection ----------------------------------------------------------

UNAME_S="$(uname -s)"
UNAME_M="$(uname -m)"
case "${UNAME_S}" in
  Darwin) OS="darwin" ;;
  Linux)  OS="linux"  ;;
  *)      die "unsupported OS: ${UNAME_S} (Windows is Tier 2, manual install)" ;;
esac
case "${UNAME_M}" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) die "unsupported arch: ${UNAME_M}" ;;
esac
log "host: ${OS}/${ARCH}"

# Binary acquisition ---------------------------------------------------------

build_from_source() {
  command -v go >/dev/null 2>&1 || die "go toolchain not found (needed for --source=build)"
  log "building from source at ${REPO_ROOT}"
  make -C "${REPO_ROOT}" build >/dev/null
}

ensure_binaries() {
  local bin_dir="${REPO_ROOT}/bin"
  case "${SOURCE_MODE}" in
    build)
      build_from_source ;;
    local)
      [[ -x "${bin_dir}/clawmast"  ]] || die "${bin_dir}/clawmast not found; run 'make build' first or use --source=build"
      [[ -x "${bin_dir}/clawmastd" ]] || die "${bin_dir}/clawmastd not found; run 'make build' first or use --source=build"
      ;;
    auto)
      if [[ -x "${bin_dir}/clawmast" && -x "${bin_dir}/clawmastd" ]]; then
        log "using pre-built binaries in ${bin_dir}"
      else
        build_from_source
      fi
      ;;
    *) die "unknown --source value: ${SOURCE_MODE}" ;;
  esac
}

# Version resolution ---------------------------------------------------------

derive_version() {
  local v
  v="$("${REPO_ROOT}/bin/clawmast" version 2>/dev/null | awk '{print $3}' | tr -d ' ' || true)"
  [[ -n "${v}" ]] || v="dev"
  # Strip leading "v" so the label matches $(git describe --tags)'s
  # bare form while the directory reads naturally.
  v="${v#v}"
  printf 'v%s' "${v}"
}

[[ -n "${VERSION_LABEL}" ]] || VERSION_LABEL=""   # defer until binaries are ready

# Templates ------------------------------------------------------------------

render_template() {
  # render_template IN OUT KEY VALUE ...
  local in="$1" out="$2"; shift 2
  local content; content="$(<"${in}")"
  while (( $# > 1 )); do
    local key="$1" val="$2"
    # Escape & and | for sed replacement safety.
    val="${val//&/\\&}"
    val="${val//|/\\|}"
    content="${content//@@${key}@@/${val}}"
    shift 2
  done
  printf '%s' "${content}" > "${out}"
}


# Install layout ------------------------------------------------------------

ensure_layout() {
  install -d -m 0700 "${PREFIX}"
  install -d -m 0700 "${PREFIX}/bin"
  install -d -m 0700 "${PREFIX}/versions"
  install -d -m 0700 "${PREFIX}/state"
  install -d -m 0700 "${PREFIX}/data"
  install -d -m 0700 "${PREFIX}/logs"
  install -d -m 0700 "${PREFIX}/run"
  install -d -m 0700 "${PREFIX}/keys"
}

seed_channel() {
  local ch="${PREFIX}/state/channel"
  if [[ ! -f "${ch}" ]]; then
    printf '%s\n' "${CHANNEL_DEFAULT}" > "${ch}"
    chmod 0600 "${ch}"
    log "seeded update channel: ${CHANNEL_DEFAULT}"
  fi
}

seed_minisign_stub() {
  local pub="${PREFIX}/keys/minisign.pub"
  if [[ ! -f "${pub}" ]]; then
    cat > "${pub}" <<'EOF'
# ClawMast minisign public key stub.
#
# Iteration 0 ships without real signing; this placeholder exists so
# the install layout matches refactor.md §5 and so the worker's
# future signature-verification path has a file to parse. Iteration 2
# replaces this with the production minisign public key.
untrusted comment: clawmast iteration-0 stub (not a valid key)
RWQ00000000000000000000000000000000000000000000000000000000000000000000
EOF
    chmod 0600 "${pub}"
    log "seeded minisign public key stub at ${pub}"
  fi
}

install_binaries() {
  local src="${REPO_ROOT}/bin"
  install -m 0755 "${src}/clawmastd" "${PREFIX}/bin/clawmastd.new"
  mv -f "${PREFIX}/bin/clawmastd.new" "${PREFIX}/bin/clawmastd"

  local vdir="${PREFIX}/versions/${VERSION_LABEL}"
  install -d -m 0700 "${vdir}"
  install -m 0755 "${src}/clawmast" "${vdir}/clawmast.new"
  mv -f "${vdir}/clawmast.new" "${vdir}/clawmast"
  log "installed worker: ${vdir}/clawmast"
}

write_manifest() {
  local vdir="${PREFIX}/versions/${VERSION_LABEL}"
  local manifest="${vdir}/MANIFEST.json"
  local sha
  sha="$(shasum -a 256 "${vdir}/clawmast" | awk '{print $1}')"
  cat > "${manifest}" <<EOF
{
  "version": "${VERSION_LABEL}",
  "os": "${OS}",
  "arch": "${ARCH}",
  "sha256": "${sha}",
  "installed_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "source": "installer-${SOURCE_MODE}"
}
EOF
  chmod 0600 "${manifest}"
}

swap_current() {
  local target="versions/${VERSION_LABEL}"
  local cur="${PREFIX}/current"
  local prev="${PREFIX}/previous"

  # Capture the outgoing pointer so auto-update's rollback has a
  # target on first upgrade too.
  if [[ -L "${cur}" ]]; then
    local existing
    existing="$(readlink "${cur}")"
    if [[ "${existing}" == "${target}" ]]; then
      log "current already points at ${target}; nothing to swap"
      return
    fi
    ln -snf "${existing}" "${prev}"
    log "rotated previous -> ${existing}"
  fi
  ln -snf "${target}" "${cur}"
  log "current -> ${target}"
}

# Services -------------------------------------------------------------------

install_launchd() {
  local plist="${HOME}/Library/LaunchAgents/com.clawmast.clawmastd.plist"
  mkdir -p "$(dirname "${plist}")"
  render_template "${TEMPLATES_DIR}/com.clawmast.clawmastd.plist.tmpl" "${plist}" \
    CLAWMASTD_BIN "${PREFIX}/bin/clawmastd" \
    CLAWMAST_HOME "${PREFIX}" \
    LOG_DIR       "${PREFIX}/logs"
  chmod 0644 "${plist}"
  log "wrote ${plist}"

  # Unload first to pick up bin/env changes; ignore failure on fresh installs.
  launchctl unload "${plist}" 2>/dev/null || true
  launchctl load   "${plist}"
  log "launchd unit loaded"
}

install_systemd_user() {
  local unit_dir="${HOME}/.config/systemd/user"
  local unit="${unit_dir}/clawmastd.service"
  mkdir -p "${unit_dir}"
  render_template "${TEMPLATES_DIR}/clawmastd.service.tmpl" "${unit}" \
    CLAWMASTD_BIN "${PREFIX}/bin/clawmastd" \
    CLAWMAST_HOME "${PREFIX}"
  chmod 0644 "${unit}"
  log "wrote ${unit}"

  systemctl --user daemon-reload
  systemctl --user enable --now clawmastd.service
  log "systemd user unit enabled + started"
}

install_service() {
  case "${OS}" in
    darwin) install_launchd ;;
    linux)
      if command -v systemctl >/dev/null 2>&1; then
        install_systemd_user
      else
        warn "systemctl not found; skipping service registration (supervisor must be launched manually)"
      fi
      ;;
  esac
}

# Main -----------------------------------------------------------------------

main() {
  ensure_binaries
  [[ -n "${VERSION_LABEL}" ]] || VERSION_LABEL="$(derive_version)"
  log "version: ${VERSION_LABEL}"
  log "prefix:  ${PREFIX}"

  if [[ -L "${PREFIX}/current" && "${FORCE}" != "yes" ]]; then
    log "upgrading existing install (prev current → previous)"
  fi

  ensure_layout
  install_binaries
  write_manifest
  swap_current
  seed_channel
  seed_minisign_stub

  if [[ "${INSTALL_SERVICE}" == "yes" ]]; then
    install_service
  else
    log "--no-service: skipped service registration"
  fi

  log "install complete at ${PREFIX}"
  log "verify: ${PREFIX}/current/clawmast version"
}

main "$@"
