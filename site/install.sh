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
# dontguess — turnkey wrapper (v0.4.2)
# Hardened: DG_HOME pin, flock start, cmdline PID verify, health-probe readiness
# Health-probe skip: only the invocation that starts the operator runs the probe.
# CF_HOME identity pin: exchange cf calls always use --cf-home $DG_HOME.
# Observability: attempt log at $DG_HOME/dontguess-attempts.log (JSONL)
set -e

DG_OP="${DG_OP:-${HOME}/.local/bin/dontguess-operator}"
# Security note (dontguess-791): DG_OP can be overridden by the calling user.
# This is a user-level CLI tool — not setuid — so any user who can set DG_OP
# can already exec arbitrary binaries through other means (PATH, aliases, etc.).
# The override is intentional and is restricted to test use only; production
# callers should never set DG_OP.  Accepting risk as documented.
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

# Attempt log paths (set early so all exit paths can log)
_ATTEMPT_LOG="${DG_HOME}/dontguess-attempts.log"
_ATTEMPT_LOCK="${DG_HOME}/dontguess-attempts.log.lock"
_ATTEMPT_CMD="${1:-}"

# Write a JSONL line to the attempt log. Args: <exit_code> <tag>
# Fail-safe: errors are silently swallowed — observability never breaks main path.
_attempt_log_write() {
  local _exit="$1" _tag="$2"
  local _ts _pid _cf_home _cwd _caller _cmd _line
  _ts=$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || true)
  _pid=$$
  _cf_home=$(printf '%s' "${CF_HOME:-}" | sed 's/"/\\"/g')
  _cwd=$(pwd 2>/dev/null | sed 's/"/\\"/g' || true)
  _cmd=$(printf '%s' "$_ATTEMPT_CMD" | sed 's/"/\\"/g')
  _caller=null
  if command -v jq >/dev/null 2>&1 && [ -n "${CF_HOME:-}" ] && [ -f "${CF_HOME}/identity.json" ]; then
    _pk=$(jq -r '.public_key // empty' "${CF_HOME}/identity.json" 2>/dev/null | cut -c1-8 2>/dev/null || true)
    case "$_pk" in
      [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) _caller="\"${_pk}\"";;
    esac
  fi
  _line="{\"ts\":\"${_ts}\",\"pid\":${_pid},\"cmd\":\"${_cmd}\",\"exit\":${_exit},\"tag\":\"${_tag}\",\"cf_home\":\"${_cf_home}\",\"cwd\":\"${_cwd}\",\"caller\":${_caller}}"
  export _DG_LOG_LINE="$_line"
  export _DG_LOG_FILE="$_ATTEMPT_LOG"
  flock "${_ATTEMPT_LOCK}" sh -c 'echo "$_DG_LOG_LINE" >> "$_DG_LOG_FILE"' 2>/dev/null || true
}

# Classify stderr content into an error tag.
_classify_tag() {
  local _exit="$1" _stderr="$2" _tag
  if [ -z "$_stderr" ] && [ "$_exit" -eq 0 ]; then
    _tag="success"
  elif printf '%s' "$_stderr" | grep -q "No exchange configured"; then
    _tag="no_exchange_configured"
  elif printf '%s' "$_stderr" | grep -qE "operator not running|server failed"; then
    _tag="operator_down"
  elif printf '%s' "$_stderr" | grep -q "identity is wrapped"; then
    _tag="identity_wrapped"
  elif printf '%s' "$_stderr" | grep -qE "not admitted|not a member"; then
    _tag="not_admitted"
  else
    _tag="other"
  fi
  printf '%s' "$_tag"
}

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
  _attempt_log_write 1 "no_exchange_configured" 2>/dev/null || true
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
#
# _i_started_operator tracks whether THIS invocation won the flock and launched
# the operator.  Only the starter runs the full health-probe loop; all other
# callers trust the starter's work and skip straight to the cf call.
_i_started_operator=0
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
    # Pin CF_HOME to DG_HOME so the operator always uses the stable exchange
    # identity, even when this wrapper was called by a subagent whose CF_HOME
    # points at a per-session directory (e.g. /tmp/cf-session-XXX).
    # Without this pin the operator inherits the caller's CF_HOME and
    # protocol.InitWithConfig() may fail or load the wrong identity (dontguess-b6e).
    nohup env CF_HOME="'"$DG_HOME"'" "'"$DG_OP"'" serve >"'"$LOG"'" 2>&1 &
    new_pid=$!
    printf "%d\n" "$new_pid" > "'"$PID_FILE"'"
    exit 0
  ' 2>/dev/null; then
    # We won the flock. Check if WE actually started the operator or found it
    # already healthy inside the flock body (flock exits 0 either way).
    _new_pid=$(cat "$PID_FILE" 2>/dev/null || true)
    if [ "$_new_pid" != "$_current_pid" ] && pid_is_operator "$_new_pid"; then
      # A new PID was written — we started it (and it's already visible).
      _i_started_operator=1
      _current_pid="$_new_pid"
    elif pid_is_operator "$_new_pid"; then
      # Flock body found it already healthy (race: another starter just finished).
      _current_pid="$_new_pid"
    else
      # We started it (PID written but process not yet visible as operator).
      _i_started_operator=1
      _current_pid="$_new_pid"
    fi
  else
    # Lost the flock — another caller is starting the operator.
    # Poll up to 5s for the PID to appear AND be verified as the operator.
    # Once it is, trust the starter's probe and proceed without re-probing.
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

# Health-probe readiness.
#
# Only the invocation that won the flock and launched the operator runs the
# full health-probe loop.  This prevents 49 concurrent callers from each
# hammering the exchange with network round-trips while the operator is still
# cold-starting.
#
# Callers that found an existing, already-verified operator skip straight to
# the cf call — the operator was already healthy before they arrived.
#
# Callers that waited for another starter trust that the starter's probe
# succeeded; they only need the PID to be alive (already verified above).
#
# Starter probe: poll every 200ms for up to 15s.
# Always uses --cf-home $DG_HOME so the probe uses the pinned exchange identity,
# not whatever CF_HOME the caller's environment has set.

if [ "$_i_started_operator" -eq 1 ]; then
  _probe_pid=$(cat "$PID_FILE" 2>/dev/null || true)
  _ready=0
  _deadline=$(( $(date +%s) + 15 ))
  while [ "$(date +%s)" -lt "$_deadline" ]; do
    if pid_is_operator "$_probe_pid" && "$CF" --cf-home "$DG_HOME" "$XCFID" buys --json >/dev/null 2>&1; then
      _ready=1
      break
    fi
    sleep 0.2 2>/dev/null || sleep 1
  done

  if [ "$_ready" -eq 0 ]; then
    echo "server failed (not ready in 15s). See $LOG" >&2
    _attempt_log_write 1 "operator_down" 2>/dev/null || true
    exit 1
  fi
fi

# Run cf, tee stderr to both terminal and capture file, classify, log, exit.
# Always pass --cf-home "$DG_HOME" so exchange operations use the pinned exchange
# identity regardless of how CF_HOME is set in the caller's environment.
# This is the key that allows subagents with per-session CF_HOME to reach the
# exchange: their CF_HOME has no identity, but DG_HOME always does.
_STDERR_TMP=$(mktemp 2>/dev/null) || _STDERR_TMP=""
if [ -n "$_STDERR_TMP" ]; then
  # POSIX-compatible stderr tee via named pipe.
  _STDERR_FIFO=$(mktemp -u 2>/dev/null || echo "${_STDERR_TMP}.fifo")
  mkfifo "$_STDERR_FIFO" 2>/dev/null || _STDERR_FIFO=""
  trap 'rm -f "$_STDERR_TMP" "$_STDERR_FIFO"' EXIT INT TERM
  if [ -n "$_STDERR_FIFO" ]; then
    tee "$_STDERR_TMP" >&2 < "$_STDERR_FIFO" &
    _TEE_PID=$!
    "$CF" --cf-home "$DG_HOME" "$XCFID" "$@" 2>"$_STDERR_FIFO"
    _CF_EXIT=$?
    wait "$_TEE_PID" 2>/dev/null || true
  else
    # Fallback: capture only, replay after
    "$CF" --cf-home "$DG_HOME" "$XCFID" "$@" 2>"$_STDERR_TMP"
    _CF_EXIT=$?
    cat "$_STDERR_TMP" >&2
  fi
  _stderr_content=$(cat "$_STDERR_TMP" 2>/dev/null || true)
  _tag=$(_classify_tag "$_CF_EXIT" "$_stderr_content")
  _attempt_log_write "$_CF_EXIT" "$_tag" 2>/dev/null || true
  exit "$_CF_EXIT"
else
  # mktemp failed; run without logging (fail-safe)
  exec "$CF" --cf-home "$DG_HOME" "$XCFID" "$@"
fi
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
