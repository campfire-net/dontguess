#!/bin/sh
# DontGuess install script
# Usage: curl -fsSL https://dontguess.ai/install.sh | sh
#
# Installs:
#   1. cf (campfire CLI) — if not already installed
#   2. dontguess-operator — exchange server binary
#   3. dontguess wrapper — turnkey CLI
#
# The wrapper auto-starts the exchange server, reads config from
# ~/.cf/dontguess-exchange.json, and routes convention operations
# (buy, put, settle) through cf.

set -e

CF_REPO="campfire-net/campfire"
DG_REPO="campfire-net/dontguess"
INSTALL_DIR="${HOME}/.local/bin"

if [ -t 1 ]; then
  GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BOLD='\033[1m'; RESET='\033[0m'
else
  GREEN=''; YELLOW=''; BOLD=''; RESET=''
fi

info()    { printf "${BOLD}%s${RESET}\n" "$1"; }
success() { printf "${GREEN}%s${RESET}\n" "$1"; }
warn()    { printf "${YELLOW}%s${RESET}\n" "$1" >&2; }
die()     { printf "\033[0;31merror: %s\033[0m\n" "$1" >&2; exit 1; }

detect_os()   { case "$(uname -s)" in Linux*) echo linux;; Darwin*) echo darwin;; *) die "Unsupported OS";; esac; }
detect_arch() { case "$(uname -m)" in x86_64|amd64) echo amd64;; aarch64|arm64) echo arm64;; *) die "Unsupported arch";; esac; }

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else echo ""; fi
}

get_latest() {
  curl -fsSL "https://api.github.com/repos/$1/releases/latest" \
    | grep '"tag_name"' | sed 's/.*"\(v[^"]*\)".*/\1/'
}

fetch_and_verify() {
  local repo="$1" name="$2" label="$3" ver="$4" tmp="$5"
  local archive="${name}_${label}.tar.gz"
  local base="https://github.com/${repo}/releases/download/${ver}"

  curl -fsSL --progress-bar -o "${tmp}/${archive}" "${base}/${archive}" \
    || die "Download failed: ${base}/${archive}"

  if curl -fsSL -o "${tmp}/ck_${name}.txt" "${base}/checksums.txt" 2>/dev/null; then
    local exp=$(grep "${archive}" "${tmp}/ck_${name}.txt" | awk '{print $1}')
    local got=$(sha256_file "${tmp}/${archive}")
    [ -n "$exp" ] && [ -n "$got" ] && [ "$got" != "$exp" ] && die "Checksum mismatch for ${archive}"
  fi

  tar -xzf "${tmp}/${archive}" -C "${tmp}"
}

main() {
  info "DontGuess installer"
  printf "\n"

  command -v curl >/dev/null 2>&1 || die "curl not found"
  command -v tar  >/dev/null 2>&1 || die "tar not found"

  OS=$(detect_os); ARCH=$(detect_arch); LABEL="${OS}_${ARCH}"
  info "Platform: ${OS}/${ARCH}"
  printf "\n"

  mkdir -p "$INSTALL_DIR"
  TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT

  # --- cf ---
  if command -v cf >/dev/null 2>&1; then
    success "  cf already installed ($(command -v cf))"
  else
    CF_VER=$(get_latest "$CF_REPO")
    [ -z "$CF_VER" ] && die "Could not find latest cf release"
    info "  Installing cf ${CF_VER}..."
    fetch_and_verify "$CF_REPO" "cf" "$LABEL" "$CF_VER" "$TMP"
    cp "${TMP}/cf_${LABEL}/cf" "${INSTALL_DIR}/cf"
    chmod +x "${INSTALL_DIR}/cf"
    success "  cf ${CF_VER} → ${INSTALL_DIR}/cf"
  fi

  # --- dontguess operator ---
  DG_VER=$(get_latest "$DG_REPO")
  [ -z "$DG_VER" ] && die "Could not find latest dontguess release"
  info "  Installing dontguess ${DG_VER}..."
  fetch_and_verify "$DG_REPO" "dontguess" "$LABEL" "$DG_VER" "$TMP"
  cp "${TMP}/dontguess_${LABEL}/dontguess" "${INSTALL_DIR}/dontguess-operator"
  chmod +x "${INSTALL_DIR}/dontguess-operator"
  success "  dontguess-operator ${DG_VER} → ${INSTALL_DIR}/dontguess-operator"

  # --- wrapper script ---
  cat > "${INSTALL_DIR}/dontguess" <<'ENDWRAPPER'
#!/bin/sh
# dontguess — turnkey wrapper (v0.4.0)
# Hardened: DG_HOME pin, flock start, cmdline PID verify, health-probe readiness
set -e

DG_OP="${DG_OP:-${HOME}/.local/bin/dontguess-operator}"
CF="${HOME}/.local/bin/cf"

# CF_HOME controls identity only (unchanged semantics).
CF_HOME="${CF_HOME:-${HOME}/.cf}"

# DG_HOME pins all singleton exchange state, independent of CF_HOME.
# Subagents with per-session CF_HOME still find the real exchange here.
DG_HOME="${DG_HOME:-${HOME}/.cf}"

CFG="${DG_HOME}/dontguess-exchange.json"
PID_FILE="${DG_HOME}/dontguess.pid"
LOG="${DG_HOME}/dontguess.log"
LOCK="${DG_HOME}/dontguess.start.lock"

case "${1:-}" in
  init|serve|convention) exec "$DG_OP" "$@";;
  join)
    shift; exec "$CF" join "$@";;
  leave) subcmd="$1"; shift; exec "$CF" "$subcmd" "$@";;
  version|--version)
    echo "dontguess wrapper"
    "$DG_OP" version 2>/dev/null || true
    "$CF" --version 2>/dev/null || true
    exit 0;;
  --help|-h|help|"")
    echo "dontguess — token-work exchange for AI agents"
    echo ""
    echo "Operator:   dontguess init | serve"
    echo "Exchange:   dontguess buy | put | settle"
    echo ""
    echo "Run 'dontguess <op> --help' for details."
    exit 0;;
esac

# Exchange config check (always from DG_HOME, not CF_HOME)
if [ ! -f "$CFG" ]; then
  echo "No exchange configured. Run: dontguess init" >&2
  exit 1
fi

# Read campfire ID (convention operations require hex ID for routing)
XCFID=$(sed -n 's/.*"exchange_campfire_id" *: *"\([^"]*\)".*/\1/p' "$CFG")
[ -z "$XCFID" ] && { echo "error: cannot read exchange_campfire_id from $CFG" >&2; exit 1; }

# PID verification helper
pid_is_operator() {
  local pid="$1"
  [ -z "$pid" ] && return 1
  kill -0 "$pid" 2>/dev/null || return 1
  local comm=""
  if [ -f "/proc/${pid}/comm" ]; then
    comm=$(cat "/proc/${pid}/comm" 2>/dev/null || true)
  else
    comm=$(ps -p "$pid" -o comm= 2>/dev/null || true)
  fi
  case "$comm" in
    dontguess-oper*) return 0;;
    *) return 1;;
  esac
}

# Auto-start with flock
_current_pid=""
if [ -f "$PID_FILE" ]; then
  _current_pid=$(cat "$PID_FILE" 2>/dev/null || true)
fi

if ! pid_is_operator "$_current_pid"; then
  if flock -n "$LOCK" sh -c '
    pid=""
    pid_file="'"$PID_FILE"'"
    [ -f "$pid_file" ] && pid=$(cat "$pid_file" 2>/dev/null || true)
    _is_op() {
      local p="$1"
      [ -z "$p" ] && return 1
      kill -0 "$p" 2>/dev/null || return 1
      local c=""
      if [ -f "/proc/${p}/comm" ]; then c=$(cat "/proc/${p}/comm" 2>/dev/null || true)
      else c=$(ps -p "$p" -o comm= 2>/dev/null || true); fi
      case "$c" in dontguess-oper*) return 0;; *) return 1;; esac
    }
    _is_op "$pid" && exit 0
    echo "Starting exchange server..." >&2
    nohup "'"$DG_OP"'" serve >"'"$LOG"'" 2>&1 &
    new_pid=$!
    printf "%d\n" "$new_pid" > "'"$PID_FILE"'"
    exit 0
  ' 2>/dev/null; then
    _current_pid=$(cat "$PID_FILE" 2>/dev/null || true)
  else
    _deadline=$(( $(date +%s) + 5 ))
    while [ "$(date +%s)" -lt "$_deadline" ]; do
      if [ -f "$PID_FILE" ]; then
        _current_pid=$(cat "$PID_FILE" 2>/dev/null || true)
        pid_is_operator "$_current_pid" && break
      fi
      sleep 0.1 2>/dev/null || sleep 1
    done
    _current_pid=$(cat "$PID_FILE" 2>/dev/null || true)
  fi
  if pid_is_operator "$_current_pid"; then
    echo "  Exchange running (pid ${_current_pid})" >&2
  fi
fi

# Health-probe readiness: verify operator PID is dontguess-operator AND exchange is readable
_probe_pid=$(cat "$PID_FILE" 2>/dev/null || true)
_ready=0
_deadline=$(( $(date +%s) + 5 ))
while [ "$(date +%s)" -lt "$_deadline" ]; do
  if pid_is_operator "$_probe_pid" && "$CF" "$XCFID" buys --json >/dev/null 2>&1; then
    _ready=1
    break
  fi
  sleep 0.1 2>/dev/null || sleep 1
done

if [ "$_ready" -eq 0 ]; then
  echo "server failed (not ready in 5s). See $LOG" >&2
  exit 1
fi

exec "$CF" "$XCFID" "$@"
ENDWRAPPER
  chmod +x "${INSTALL_DIR}/dontguess"
  success "  dontguess (wrapper) → ${INSTALL_DIR}/dontguess"

  # PATH
  printf "\n"
  case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) success "${INSTALL_DIR} is in your PATH.";;
    *) warn "${INSTALL_DIR} is not in your PATH."
       printf "  echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.profile\n\n";;
  esac

  success "Done!"
  printf "\n"
  printf "  dontguess init                 # create an exchange\n"
  printf "  dontguess buy --task \"...\"      # search before computing\n"
  printf "  dontguess put --description ... # sell after computing\n"
  printf "\n"
}

main "$@"
