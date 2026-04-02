#!/bin/sh
# DontGuess install script
# Usage: curl -fsSL https://dontguess.ai/install.sh | sh
#
# Installs:
#   1. cf (campfire CLI) — the protocol layer
#   2. dontguess operator binary — exchange server
#   3. dontguess wrapper — turnkey CLI that handles everything
#
# The wrapper script is the user-facing entry point. It:
#   - Starts the exchange server if it's not running
#   - Creates the exchange campfire if it doesn't exist
#   - Warns if no operator identity is configured
#   - Routes convention operations (buy, put, settle) through cf multicall
#   - Routes operator commands (serve, init) through the operator binary

set -e

CF_REPO="campfire-net/campfire"
DG_REPO="campfire-net/dontguess"
INSTALL_DIR="${HOME}/.local/bin"
DG_HOME="${HOME}/.dontguess"

# Colors (only if terminal supports them)
if [ -t 1 ]; then
  RED='\033[0;31m'
  GREEN='\033[0;32m'
  YELLOW='\033[1;33m'
  BOLD='\033[1m'
  RESET='\033[0m'
else
  RED=''
  GREEN=''
  YELLOW=''
  BOLD=''
  RESET=''
fi

info()    { printf "${BOLD}%s${RESET}\n" "$1"; }
success() { printf "${GREEN}%s${RESET}\n" "$1"; }
warn()    { printf "${YELLOW}%s${RESET}\n" "$1" >&2; }
die()     { printf "${RED}error: %s${RESET}\n" "$1" >&2; exit 1; }

detect_os() {
  case "$(uname -s)" in
    Linux*)  echo "linux" ;;
    Darwin*) echo "darwin" ;;
    *)       die "Unsupported OS: $(uname -s)" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) die "Unsupported architecture: $(uname -m)" ;;
  esac
}

check_deps() {
  for cmd in curl tar; do
    command -v "$cmd" >/dev/null 2>&1 || die "Required tool not found: $cmd"
  done
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    echo ""
  fi
}

get_latest_version() {
  local repo="$1"
  local version
  version=$(curl -fsSL "https://api.github.com/repos/${repo}/releases/latest" \
    | grep '"tag_name"' | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')
  [ -z "$version" ] && die "Could not determine latest version for ${repo}"
  echo "$version"
}

install_binary() {
  local repo="$1" name="$2" label="$3" version="$4" tmp="$5"
  local archive="${name}_${label}.tar.gz"
  local url="https://github.com/${repo}/releases/download/${version}/${archive}"
  local checksums_url="https://github.com/${repo}/releases/download/${version}/checksums.txt"

  info "  Downloading ${name} ${version}..."
  curl -fsSL --progress-bar -o "${tmp}/${archive}" "$url" \
    || die "Download failed: ${url}"

  # Verify checksum if available
  if curl -fsSL -o "${tmp}/checksums_${name}.txt" "$checksums_url" 2>/dev/null; then
    local expected actual
    expected=$(grep "${archive}" "${tmp}/checksums_${name}.txt" | awk '{print $1}')
    if [ -n "$expected" ]; then
      actual=$(sha256_file "${tmp}/${archive}")
      if [ -n "$actual" ] && [ "$actual" != "$expected" ]; then
        die "Checksum mismatch for ${archive}"
      fi
    fi
  fi

  tar -xzf "${tmp}/${archive}" -C "${tmp}"
}

write_wrapper() {
  cat > "${INSTALL_DIR}/dontguess" << 'WRAPPER'
#!/bin/sh
# dontguess — turnkey wrapper for the DontGuess token-work exchange.
#
# Convention operations (buy, put, settle, ...) route through cf multicall.
# Operator commands (init, serve) route through the operator binary.
# If the exchange isn't running, starts it. If it doesn't exist, creates it.

set -e

DG_HOME="${HOME}/.dontguess"
DG_OPERATOR="${HOME}/.local/bin/dontguess-operator"
CF_BIN="${HOME}/.local/bin/cf"
PIDFILE="${DG_HOME}/exchange.pid"
LOGFILE="${DG_HOME}/exchange.log"

# Operator commands — pass through to the operator binary
case "${1:-}" in
  init|serve|convention)
    exec "$DG_OPERATOR" "$@"
    ;;
  version|--version)
    printf "dontguess wrapper\n"
    [ -x "$DG_OPERATOR" ] && printf "  operator: " && "$DG_OPERATOR" version 2>/dev/null || true
    [ -x "$CF_BIN" ] && printf "  cf:       " && "$CF_BIN" --version 2>/dev/null || true
    exit 0
    ;;
esac

# --- Turnkey checks: make sure the exchange is ready ---

# 1. Operator identity
if [ ! -d "${DG_HOME}" ]; then
  printf "\033[1;33mwarning: no dontguess home directory (~/.dontguess)\033[0m\n" >&2
  printf "  Run: dontguess init\n" >&2
  exit 1
fi

# 2. Exchange campfire — check if initialized
if [ ! -f "${DG_HOME}/exchange.campfire" ]; then
  printf "\033[1;33mwarning: no exchange campfire configured\033[0m\n" >&2
  printf "  Run: dontguess init --name my-exchange\n" >&2
  printf "  Or:  dontguess join <campfire-id>\n" >&2
  exit 1
fi

EXCHANGE_CF=$(cat "${DG_HOME}/exchange.campfire")

# 3. Exchange server — start if not running
if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
  : # server is running
else
  printf "\033[1mStarting exchange server...\033[0m\n" >&2
  nohup "$DG_OPERATOR" serve > "$LOGFILE" 2>&1 &
  echo $! > "$PIDFILE"
  sleep 1
  if ! kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
    printf "\033[0;31merror: exchange server failed to start. Check %s\033[0m\n" "$LOGFILE" >&2
    exit 1
  fi
  printf "\033[0;32m  Exchange running (pid %s)\033[0m\n" "$(cat "$PIDFILE")" >&2
fi

# --- Route convention operations through cf multicall ---
# cf multicall uses argv[0] as the convention name, so we exec cf
# with "dontguess" as the program name by symlinking. But since this
# wrapper IS "dontguess", we call cf directly with the convention prefix.

exec "$CF_BIN" "$EXCHANGE_CF" "$@"
WRAPPER
  chmod +x "${INSTALL_DIR}/dontguess"
}

main() {
  info "DontGuess installer"
  printf "\n"

  check_deps

  OS=$(detect_os)
  ARCH=$(detect_arch)
  LABEL="${OS}_${ARCH}"

  info "Platform: ${OS}/${ARCH}"
  printf "\n"

  mkdir -p "$INSTALL_DIR" "$DG_HOME"
  TMP_DIR=$(mktemp -d)
  trap 'rm -rf "$TMP_DIR"' EXIT

  # --- Install cf ---
  if command -v cf >/dev/null 2>&1; then
    success "  cf already installed ($(command -v cf))"
  else
    CF_VERSION=$(get_latest_version "$CF_REPO")
    install_binary "$CF_REPO" "cf" "$LABEL" "$CF_VERSION" "$TMP_DIR"

    CF_BIN="${TMP_DIR}/cf_${LABEL}/cf"
    [ -f "$CF_BIN" ] || die "cf binary not found in archive"
    cp "$CF_BIN" "${INSTALL_DIR}/cf"
    chmod +x "${INSTALL_DIR}/cf"
    success "  cf ${CF_VERSION} → ${INSTALL_DIR}/cf"
  fi

  # --- Install dontguess operator binary ---
  DG_VERSION=$(get_latest_version "$DG_REPO")
  install_binary "$DG_REPO" "dontguess" "$LABEL" "$DG_VERSION" "$TMP_DIR"

  DG_BIN="${TMP_DIR}/dontguess_${LABEL}/dontguess"
  [ -f "$DG_BIN" ] || die "dontguess operator binary not found in archive"
  cp "$DG_BIN" "${INSTALL_DIR}/dontguess-operator"
  chmod +x "${INSTALL_DIR}/dontguess-operator"
  success "  dontguess-operator ${DG_VERSION} → ${INSTALL_DIR}/dontguess-operator"

  # --- Write the wrapper script ---
  write_wrapper
  success "  dontguess (wrapper) → ${INSTALL_DIR}/dontguess"

  # PATH advice
  printf "\n"
  case ":${PATH}:" in
    *":${INSTALL_DIR}:"*)
      success "${INSTALL_DIR} is in your PATH."
      ;;
    *)
      warn "${INSTALL_DIR} is not in your PATH."
      printf "\n  echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.profile && source ~/.profile\n\n"
      ;;
  esac

  printf "\n"
  success "Done!"
  printf "\n"
  info "Quick start:"
  printf "\n"
  printf "  dontguess init                 # create an exchange\n"
  printf "  dontguess buy --task \"...\"      # search before computing\n"
  printf "  dontguess put --description ... # sell after computing\n"
  printf "\n"
}

main "$@"
