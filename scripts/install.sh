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

# Default install root follows the XDG Base Directory spec
# (~/.local/share/<app>) so binaries live under a standard data
# directory and symlinks in ~/.local/bin remain the discoverable
# surface. Legacy ~/.clawmast/ installs keep working: resolve_prefix
# below prefers an existing ~/.clawmast/current over the XDG default
# so re-running install.sh on an old host is an in-place upgrade, not
# a silent relocation.
DEFAULT_PREFIX_XDG="${HOME}/.local/share/clawmast"
LEGACY_PREFIX="${HOME}/.clawmast"
PREFIX="${CLAWMAST_HOME:-}"   # empty => resolve below
VERSION_LABEL=""
SOURCE_MODE="auto"      # auto | local | build
BIN_DIR=""              # override of ${REPO_ROOT}/bin; see --bin-dir
INSTALL_SERVICE="yes"
# INSTALL_SYMLINKS stays unset here so we can distinguish "user didn't
# say" from "user said yes". main() below flips the default to "no"
# when --no-service is set, so test/playground installs under /tmp
# never plant symlinks into the host's /usr/local/bin.
INSTALL_SYMLINKS=""
FORCE="no"
CHANNEL_DEFAULT="stable"

usage() {
  cat <<'EOF'
Usage: install.sh [options]

Options:
  --prefix PATH           Install root. Default resolution order:
                          (1) $CLAWMAST_HOME if set;
                          (2) ~/.clawmast if it already contains an
                              install (legacy compat);
                          (3) ~/.local/share/clawmast (XDG).
  --version LABEL         Version directory name under versions/ (default: derived)
  --source {auto|local|build}
                          auto  — prefer ./bin, fall back to `go build` (default)
                          local — require pre-built binaries in ./bin
                          build — always rebuild from this repo
  --bin-dir PATH          Read pre-built clawmast / clawmastd from PATH
                          instead of ./bin. Forces --source=local.
  --no-service            Skip writing / enabling the launchd / systemd unit.
                          Implies --no-symlinks unless --symlinks is also set.
  --symlinks              Force creating CLI symlinks even with --no-service.
  --no-symlinks           Skip creating /usr/local/bin or ~/.local/bin
                          CLI symlinks. The UI and service still work; the
                          clawmast / clawmastd commands just stay under
                          the install root.
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
    --bin-dir)    BIN_DIR="$2"; SOURCE_MODE="local"; shift 2 ;;
    --no-service) INSTALL_SERVICE="no"; shift ;;
    --symlinks)    INSTALL_SYMLINKS="yes"; shift ;;
    --no-symlinks) INSTALL_SYMLINKS="no"; shift ;;
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

# Prefix resolution ----------------------------------------------------------
#
# Precedence, applied only when --prefix was not passed explicitly and
# CLAWMAST_HOME is unset:
#   1. ~/.clawmast already contains a valid install -> use it (legacy).
#   2. Otherwise -> ~/.local/share/clawmast (XDG data dir).
#
# We detect a "valid install" by the presence of ./current (the symlink
# install.sh itself plants). That keeps mere stray files in the legacy
# path from dragging us back there.
resolve_prefix() {
  if [[ -n "${PREFIX}" ]]; then
    return
  fi
  if [[ -L "${LEGACY_PREFIX}/current" ]]; then
    PREFIX="${LEGACY_PREFIX}"
    warn "found legacy install at ${LEGACY_PREFIX}; continuing in place"
    warn "run 'bash scripts/install.sh --prefix ${DEFAULT_PREFIX_XDG}' to migrate later"
    return
  fi
  PREFIX="${DEFAULT_PREFIX_XDG}"
}
resolve_prefix

# CLI symlink dir resolution -------------------------------------------------
#
# Prefers /usr/local/bin when writable (zero PATH setup on macOS with
# Homebrew, standard on Linux) and falls back to ~/.local/bin (XDG).
# Writing the choice to state/cli-bin-dir lets uninstall.sh remove the
# symlinks without second-guessing the original selection.
pick_cli_bin_dir() {
  if [[ "${INSTALL_SYMLINKS}" != "yes" ]]; then
    echo ""
    return
  fi
  local candidates=( "/usr/local/bin" "${HOME}/.local/bin" )
  for d in "${candidates[@]}"; do
    if [[ -d "${d}" && -w "${d}" ]]; then
      echo "${d}"; return
    fi
    # ~/.local/bin may not exist yet; we can create it without sudo.
    if [[ "${d}" == "${HOME}/.local/bin" ]]; then
      echo "${d}"; return
    fi
  done
  echo ""
}

# Binary acquisition ---------------------------------------------------------

build_from_source() {
  command -v go >/dev/null 2>&1 || die "go toolchain not found (needed for --source=build)"
  log "building from source at ${REPO_ROOT}"
  make -C "${REPO_ROOT}" build >/dev/null
}

ensure_binaries() {
  if [[ -z "${BIN_DIR}" ]]; then
    BIN_DIR="${REPO_ROOT}/bin"
  fi
  case "${SOURCE_MODE}" in
    build)
      [[ "${BIN_DIR}" == "${REPO_ROOT}/bin" ]] \
        || die "--bin-dir is incompatible with --source=build"
      build_from_source ;;
    local)
      [[ -x "${BIN_DIR}/clawmast"  ]] || die "${BIN_DIR}/clawmast not found; run 'make build' first or use --source=build"
      [[ -x "${BIN_DIR}/clawmastd" ]] || die "${BIN_DIR}/clawmastd not found; run 'make build' first or use --source=build"
      ;;
    auto)
      if [[ -x "${BIN_DIR}/clawmast" && -x "${BIN_DIR}/clawmastd" ]]; then
        log "using pre-built binaries in ${BIN_DIR}"
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
  v="$("${BIN_DIR}/clawmast" version 2>/dev/null | awk '{print $3}' | tr -d ' ' || true)"
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
  local src="${BIN_DIR}"
  local dst="${PREFIX}/bin/clawmastd"

  # Supervisor upgrade safety net: if a clawmastd already exists, keep
  # a backup so a bad rollout can be reversed without re-running the
  # whole installer. The .new → mv pattern below is atomic on the same
  # filesystem, so a successful mv overwrites the binary in one inode
  # swap; the smoke test then confirms the replacement is runnable
  # before we touch anything else (unit reload, current symlink, …).
  if [[ -x "${dst}" ]]; then
    cp -f "${dst}" "${dst}.bak"
  fi

  install -m 0755 "${src}/clawmastd" "${dst}.new"
  mv -f "${dst}.new" "${dst}"

  if ! "${dst}" version >/dev/null 2>&1; then
    if [[ -x "${dst}.bak" ]]; then
      warn "new clawmastd failed smoke test; reverting to previous binary"
      mv -f "${dst}.bak" "${dst}"
      die "supervisor upgrade aborted (rolled back); investigate ${src}/clawmastd"
    fi
    die "new clawmastd failed smoke test and no backup was available"
  fi
  rm -f "${dst}.bak"

  local vdir="${PREFIX}/versions/${VERSION_LABEL}"
  install -d -m 0700 "${vdir}"
  install -m 0755 "${src}/clawmast" "${vdir}/clawmast.new"
  mv -f "${vdir}/clawmast.new" "${vdir}/clawmast"
  log "installed worker: ${vdir}/clawmast"

  # On macOS, strip the com.apple.quarantine xattr Gatekeeper sets on
  # files that arrived via LaunchServices (browsers, AirDrop). Go's
  # net/http auto-updater path does not set it, so this is a no-op for
  # auto-update; it only matters when the user downloaded a release
  # tarball in Safari and is running install.sh out of ~/Downloads.
  # Failure is non-fatal — xattr may be absent in minimal shells.
  if [[ "${OS}" == "darwin" ]] && command -v xattr >/dev/null 2>&1; then
    xattr -dr com.apple.quarantine "${PREFIX}/bin/clawmastd" 2>/dev/null || true
    xattr -dr com.apple.quarantine "${vdir}/clawmast"        2>/dev/null || true
  fi
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
    CLAWMASTD_BIN  "${PREFIX}/bin/clawmastd" \
    CLAWMAST_HOME  "${PREFIX}" \
    LOG_DIR        "${PREFIX}/logs" \
    HOME_LOCAL_BIN "${HOME}/.local/bin"
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
    CLAWMASTD_BIN  "${PREFIX}/bin/clawmastd" \
    CLAWMAST_HOME  "${PREFIX}" \
    HOME_LOCAL_BIN "${HOME}/.local/bin"
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

# CLI symlinks ---------------------------------------------------------------
#
# Planting clawmast + clawmastd on PATH is what turns "a daemon somewhere"
# into a command the user can actually type. The chosen directory is
# recorded under state/cli-bin-dir so uninstall.sh can sweep the links
# back out without re-guessing our preference order. Failures here are
# advisory, not fatal: the service still runs; only the CLI shortcut is
# missing, and the message tells the user exactly which fallback to add
# to PATH.
install_cli_symlinks() {
  local target_dir
  target_dir="$(pick_cli_bin_dir)"
  if [[ -z "${target_dir}" ]]; then
    log "--no-symlinks: skipped CLI shortcut creation"
    rm -f "${PREFIX}/state/cli-bin-dir"
    return
  fi

  if ! mkdir -p "${target_dir}" 2>/dev/null; then
    warn "cannot create ${target_dir}; skipping CLI shortcuts"
    return
  fi
  if [[ ! -w "${target_dir}" ]]; then
    warn "no write access to ${target_dir}; skipping CLI shortcuts"
    warn "add ${PREFIX}/current and ${PREFIX}/bin to PATH manually if needed"
    return
  fi

  ln -snf "${PREFIX}/current/clawmast"  "${target_dir}/clawmast"
  ln -snf "${PREFIX}/bin/clawmastd"     "${target_dir}/clawmastd"
  printf '%s\n' "${target_dir}" > "${PREFIX}/state/cli-bin-dir"
  chmod 0600 "${PREFIX}/state/cli-bin-dir"
  log "symlinked clawmast + clawmastd into ${target_dir}"

  # ~/.local/bin is not on PATH by default on macOS; nudge the user
  # rather than silently leaving `clawmast status` unreachable.
  case ":${PATH}:" in
    *":${target_dir}:"*) ;;
    *) warn "${target_dir} is not on PATH; add it to your shell rc to use 'clawmast' directly" ;;
  esac
}

# Main -----------------------------------------------------------------------

main() {
  # Default symlink policy: track --no-service. Test/playground installs
  # under /tmp run headless and should not plant links into the host's
  # /usr/local/bin; a real user install registers the service and gets
  # the links planted. --symlinks / --no-symlinks override either way.
  if [[ -z "${INSTALL_SYMLINKS}" ]]; then
    if [[ "${INSTALL_SERVICE}" == "yes" ]]; then
      INSTALL_SYMLINKS="yes"
    else
      INSTALL_SYMLINKS="no"
    fi
  fi

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

  install_cli_symlinks

  log "install complete at ${PREFIX}"
  if [[ -f "${PREFIX}/state/cli-bin-dir" ]]; then
    log "verify: clawmast version   (or: ${PREFIX}/current/clawmast version)"
  else
    log "verify: ${PREFIX}/current/clawmast version"
  fi
}

main "$@"
