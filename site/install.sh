#!/bin/sh
# DontGuess install script
# Usage: curl -fsSL https://dontguess.ai/install.sh | sh
#
# Installs:
#   1. dontguess-operator — the exchange binary (operator serve + client verbs
#      put / buy / settle-driven-by-buy, all in one static Go binary)
#   2. dontguess wrapper — turnkey CLI
#
# Nostr-first (dontguess-ed2): there is NO campfire (cf) dependency. The client
# publishes agent-signed events directly to the team relay (team tier) or routes
# through the single local `serve` over the operator IPC socket (individual
# tier). Tier is selected by DONTGUESS_RELAY_URLS: non-empty ⇒ team, empty ⇒
# individual. The wrapper auto-starts the exchange server on the INDIVIDUAL tier
# ONLY (H6): in team tier the client uses the provisioned operator relay and
# never auto-starts a local operator.

set -e

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

  # --- dontguess operator (also the client binary) ---
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
# dontguess — turnkey wrapper (v0.7.0, nostr-first)
#
# Nostr-first (dontguess-ed2): NO campfire (cf) dependency. Every verb dispatches
# to the single dontguess-operator binary (operator serve + client put/buy). The
# client reads DONTGUESS_RELAY_URLS (tier), AGENT_CF_HOME (agent signing key), and
# DG_HOME (exchange home) from the environment itself — the wrapper only sets up
# DG_HOME, the individual-tier auto-start, synthetic tagging, and attempt logging.
#
# Tier detection mirrors the operator (design §3.3): DONTGUESS_RELAY_URLS non-empty
#   ⇒ TEAM (client dials the relay directly, agent-signed); empty ⇒ INDIVIDUAL
#   (client routes through the single local `serve` over the IPC socket).
#
# H6 (design §3.10): the flock serve auto-start is gated on the INDIVIDUAL tier
#   ONLY. In team tier the client uses the provisioned operator relay and MUST
#   NEVER auto-start a local operator — a client-spawned relay-attached serve mints
#   its own key and becomes a rogue competing sequencer.
#
# DG_HOME pins all singleton exchange state (operator identity, event log, IPC
#   socket, PID). Defaults to ~/.dontguess to match dgpath.go resolveDGHome.
# AGENT_CF_HOME: per-agent secp256k1 signing identity for buy/put. The operator
#   binary reads it directly; the wrapper records the signing agent for telemetry.
# Observability: attempt log at $DG_HOME/dontguess-attempts.log (JSONL).
# DG_SYNTHETIC: dev/CI synthetic-tag injection (dontguess-18c).
#   Auto-enabled when DG_SYNTHETIC=1 or CI env var is set. Opt-out: DG_SYNTHETIC=0.
#   Effect: buy/put calls get --synthetic appended so the engine tags responses
#   exchange:synthetic, excluding them from real exchange metrics/inventory.
set -e

DG_OP="${DG_OP:-${HOME}/.local/bin/dontguess-operator}"
# Security note (dontguess-791): DG_OP can be overridden by the calling user.
# This is a user-level CLI tool — not setuid — so any user who can set DG_OP
# can already exec arbitrary binaries through other means (PATH, aliases, etc.).
# The override is intentional and is restricted to test use only; production
# callers should never set DG_OP.  Accepting risk as documented.

# DG_HOME pins all singleton exchange state, independent of any per-session env.
# Default matches the operator binary (cmd/dontguess/dgpath.go resolveDGHome):
# ~/.dontguess. Subagents with a per-session AGENT_CF_HOME still find the real
# exchange here.
DG_HOME="${DG_HOME:-${HOME}/.dontguess}"

CFG="${DG_HOME}/dontguess-exchange.json"
PID_FILE="${DG_HOME}/dontguess.pid"
LOG="${DG_HOME}/dontguess.log"
LOCK="${DG_HOME}/dontguess.start.lock"
SOCK="${DG_HOME}/ipc/dontguess.sock"

# Attempt log paths (set early so all exit paths can log)
_ATTEMPT_LOG="${DG_HOME}/dontguess-attempts.log"
_ATTEMPT_LOCK="${DG_HOME}/dontguess-attempts.log.lock"
_ATTEMPT_CMD="${1:-}"

# Write a JSONL line to the attempt log. Args: <exit_code> <tag>
# Fail-safe: errors are silently swallowed — observability never breaks main path.
# Schema (consumed by status.go attemptLine): ts, pid, cmd, exit, tag, cf_home,
# cwd, caller. The cf_home field is retained for schema stability but now carries
# DG_HOME (the exchange home); caller is the signing agent's pubkey when known.
_attempt_log_write() {
  local _exit="$1" _tag="$2"
  local _ts _pid _cf_home _cwd _caller _cmd _line
  _ts=$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || true)
  _pid=$$
  _cf_home=$(printf '%s' "${DG_HOME:-}" | sed 's/"/\\"/g')
  _cwd=$(pwd 2>/dev/null | sed 's/"/\\"/g' || true)
  _cmd=$(printf '%s' "$_ATTEMPT_CMD" | sed 's/"/\\"/g')
  _caller=null
  # Prefer the AGENT_CF_HOME nostr identity (the actual signing agent).
  _id_src=""
  if [ -n "${AGENT_CF_HOME:-}" ] && [ -f "${AGENT_CF_HOME}/nostr-identity.json" ]; then
    _id_src="${AGENT_CF_HOME}/nostr-identity.json"
  fi
  if command -v jq >/dev/null 2>&1 && [ -n "$_id_src" ]; then
    _pk=$(jq -r '.pub_key_hex // .public_key // empty' "$_id_src" 2>/dev/null | cut -c1-8 2>/dev/null || true)
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
  elif printf '%s' "$_stderr" | grep -qE "operator not running|operator not reachable|server failed"; then
    _tag="operator_down"
  elif printf '%s' "$_stderr" | grep -qE "AGENT_CF_HOME is not set|resolve agent identity"; then
    _tag="identity_missing"
  elif printf '%s' "$_stderr" | grep -qE "not allowlisted|put-reject|dropped_unlisted|underfunded"; then
    _tag="not_admitted"
  else
    _tag="other"
  fi
  printf '%s' "$_tag"
}

case "${1:-}" in
  # Operator / management / identity commands: dispatch straight to the binary.
  # None of these auto-start a local serve (H6): they are operator-local actions
  # run where the operator is already provisioned, or self-contained client setup.
  init|serve|agent-init|allowlist|mint|status|demand|hit-rate|operator|convention)
    exec "$DG_OP" "$@";;
  version|--version)
    echo "dontguess wrapper"
    "$DG_OP" version 2>/dev/null || true
    exit 0;;
  upgrade)
    echo "Upgrading dontguess to the latest release..."
    curl -fsSL https://dontguess.ai/install.sh | sh
    exit 0;;
  --help|-h|help|"")
    echo "dontguess — token-work exchange for AI agents (nostr-first)"
    echo ""
    echo "Operator:   dontguess init | serve | allowlist | mint | status"
    echo "Exchange:   dontguess buy | put    (settle is driven by buy on a hit)"
    echo "Agent key:  eval \$(dontguess agent-init <name> --fleet-member)"
    echo "Upgrade:    dontguess upgrade"
    echo ""
    echo "Tier: set DONTGUESS_RELAY_URLS for the team tier (client dials the relay"
    echo "      directly); leave it unset for the individual tier (local serve)."
    echo ""
    echo "Run 'dontguess <op> --help' for details."
    echo ""
    echo "High-value puts (12-37x reuse in practice):"
    echo "  - Checklists and validation patterns (schema correctness, conformance CI filters)"
    echo "  - Cross-project setup knowledge (README excerpts, migration recipes, config idioms)"
    echo "  - Language-level idioms (Go flock contention pattern, test harness patterns)"
    echo "  - Reusable code fragments (CI path filters, store bridge scripts)"
    echo ""
    echo "Skip: session-specific analysis, one-off derivations, 'test'/'smoke-test' entries,"
    echo "      load-test traffic, or anything with token_cost < 500."
    echo ""
    echo "The exchange earns you 10% residuals per re-sale — put what others will re-derive."
    exit 0;;
esac

# --- exchange hot path (buy / put) below ---

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

# H6 (design §3.10): the flock serve auto-start is gated on the INDIVIDUAL tier
# ONLY. When DONTGUESS_RELAY_URLS is set (team tier) the client dials the relay
# directly and this whole block is skipped — the wrapper NEVER auto-starts a local
# operator in team tier, so a rogue competing sequencer cannot be spawned.
if [ -z "${DONTGUESS_RELAY_URLS:-}" ]; then
  # Individual tier: buy/put route through the single local serve over the IPC
  # socket. The exchange must be bootstrapped first (`dontguess init`).
  if [ ! -f "$CFG" ]; then
    echo "No exchange configured. Run: dontguess init" >&2
    _attempt_log_write 1 "no_exchange_configured" 2>/dev/null || true
    exit 1
  fi

  # Auto-start with flock (single-writer guarantee: exactly one serve owns the
  # event log). _i_started_operator tracks whether THIS invocation launched the
  # operator; only the starter runs the readiness probe.
  _i_started_operator=0
  _current_pid=""
  if [ -f "$PID_FILE" ]; then
    _current_pid=$(cat "$PID_FILE" 2>/dev/null || true)
  fi

  if ! pid_is_operator "$_current_pid"; then
    # Pass all values to the flock subshell via the environment, never by
    # string-interpolating them into the `sh -c` command text (dontguess-732).
    # The body below is a fully single-quoted literal: the shell performs NO
    # expansion on it, so a DG_HOME / DG_OP containing single quotes or other
    # shell metacharacters cannot break out and inject commands. Inside the
    # subshell the values are read back as ordinary "$VAR" references.
    if _DG_PID_FILE="$PID_FILE" _DG_HOME="$DG_HOME" _DG_OP="$DG_OP" _DG_LOG="$LOG" \
       flock -n "$LOCK" sh -c '
      pid=""
      pid_file="$_DG_PID_FILE"
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
      # Pin DG_HOME so the operator always uses the stable exchange home, even
      # when this wrapper was called by a subagent whose environment points DG_HOME
      # elsewhere. The operator resolves DG_HOME from the environment (dgpath.go).
      nohup env DG_HOME="$_DG_HOME" "$_DG_OP" serve >"$_DG_LOG" 2>&1 &
      new_pid=$!
      printf "%d\n" "$new_pid" > "$_DG_PID_FILE"
      exit 0
    ' 2>/dev/null; then
      # We won the flock. Check whether WE started the operator or found it
      # already healthy inside the flock body (flock exits 0 either way).
      _new_pid=$(cat "$PID_FILE" 2>/dev/null || true)
      if [ "$_new_pid" != "$_current_pid" ] && pid_is_operator "$_new_pid"; then
        _i_started_operator=1
        _current_pid="$_new_pid"
      elif pid_is_operator "$_new_pid"; then
        _current_pid="$_new_pid"
      else
        _i_started_operator=1
        _current_pid="$_new_pid"
      fi
    else
      # Lost the flock — another caller is starting the operator. Poll up to 5s
      # for the PID to appear AND be verified as the operator, then trust it.
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

  # Readiness: only the invocation that started the operator waits for it. The
  # serve creates its IPC socket just before it begins folding (serve.go
  # listenOperatorSocket), so the socket appearing is the readiness signal. This
  # is campfire-free — no network probe. If the operator PID dies, stop waiting
  # and fail LOUD rather than hang the full window.
  if [ "$_i_started_operator" -eq 1 ]; then
    _probe_pid=$(cat "$PID_FILE" 2>/dev/null || true)
    _ready=0
    _deadline=$(( $(date +%s) + 15 ))
    while [ "$(date +%s)" -lt "$_deadline" ]; do
      if ! pid_is_operator "$_probe_pid"; then
        break
      fi
      if [ -S "$SOCK" ]; then
        _ready=1
        break
      fi
      sleep 0.2 2>/dev/null || sleep 1
    done

    if [ "$_ready" -eq 0 ]; then
      echo "server failed (operator socket not ready in 15s). See $LOG" >&2
      _attempt_log_write 1 "operator_down" 2>/dev/null || true
      exit 1
    fi
  fi
fi

# Synthetic-tag injection (dontguess-18c).
# Detect dev/CI context and inject --synthetic into buy/put calls so the engine
# tags its responses exchange:synthetic, excluding them from real metrics.
#   DG_SYNTHETIC=0 → always off (explicit opt-out, strongest signal)
#   DG_SYNTHETIC=1 → always on (explicit opt-in)
#   CI non-empty  → on (common CI env var set by GitHub Actions, CircleCI, etc.)
#   --synthetic in args → on (caller passed it directly; wrapper deduplicates)
#   default       → off (no marker present)
_DG_INJECT_SYNTHETIC=0
case "${DG_SYNTHETIC:-}" in
  0) _DG_INJECT_SYNTHETIC=0 ;;
  1) _DG_INJECT_SYNTHETIC=1 ;;
  *)
    if [ -n "${CI:-}" ]; then
      _DG_INJECT_SYNTHETIC=1
    fi
    for _a in "$@"; do
      if [ "$_a" = "--synthetic" ]; then
        _DG_INJECT_SYNTHETIC=0  # already present; don't double-inject
        break
      fi
    done
    ;;
esac

# Build the final argument list, injecting --synthetic for buy/put when active.
# settle is excluded: settlement of real inventory is always real traffic.
if [ "$_DG_INJECT_SYNTHETIC" -eq 1 ]; then
  case "${1:-}" in
    buy|put) set -- "$@" --synthetic ;;
  esac
fi

# Dispatch to the dontguess binary subcommand (NOT cf). The binary reads
# DONTGUESS_RELAY_URLS (tier), AGENT_CF_HOME (agent key), and DG_HOME from the
# environment itself. Tee stderr to both the terminal and a capture file,
# classify, attempt-log, exit.
_STDERR_TMP=$(mktemp 2>/dev/null) || _STDERR_TMP=""
if [ -n "$_STDERR_TMP" ]; then
  # POSIX-compatible stderr tee via named pipe.
  _STDERR_FIFO=$(mktemp -u 2>/dev/null || echo "${_STDERR_TMP}.fifo")
  mkfifo "$_STDERR_FIFO" 2>/dev/null || _STDERR_FIFO=""
  trap 'rm -f "$_STDERR_TMP" "$_STDERR_FIFO"' EXIT INT TERM
  if [ -n "$_STDERR_FIFO" ]; then
    tee "$_STDERR_TMP" >&2 < "$_STDERR_FIFO" &
    _TEE_PID=$!
    "$DG_OP" "$@" 2>"$_STDERR_FIFO"
    _DG_EXIT=$?
    wait "$_TEE_PID" 2>/dev/null || true
  else
    # Fallback: capture only, replay after
    "$DG_OP" "$@" 2>"$_STDERR_TMP"
    _DG_EXIT=$?
    cat "$_STDERR_TMP" >&2
  fi
  _stderr_content=$(cat "$_STDERR_TMP" 2>/dev/null || true)
  _tag=$(_classify_tag "$_DG_EXIT" "$_stderr_content")
  _attempt_log_write "$_DG_EXIT" "$_tag" 2>/dev/null || true
  exit "$_DG_EXIT"
else
  # mktemp failed; run without logging (fail-safe)
  exec "$DG_OP" "$@"
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
