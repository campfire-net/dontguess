#!/bin/sh
# DontGuess install script
# Usage: curl -fsSL https://dontguess.ai/install.sh | sh
#
# Installs dontguess and seller to ~/.local/bin

set -e

REPO="campfire-net/dontguess"
INSTALL_DIR="${HOME}/.local/bin"

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
    *)       die "Unsupported OS: $(uname -s). Download manually from https://github.com/${REPO}/releases" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) die "Unsupported architecture: $(uname -m). Download manually from https://github.com/${REPO}/releases" ;;
  esac
}

check_deps() {
  for cmd in curl tar; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
      die "Required tool not found: $cmd"
    fi
  done
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    warn "No sha256 tool found — skipping verification"
    echo ""
  fi
}

get_latest_version() {
  local url="https://api.github.com/repos/${REPO}/releases/latest"
  local version

  version=$(curl -fsSL "$url" | grep '"tag_name"' | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')

  if [ -z "$version" ]; then
    die "Could not determine latest version. Check your internet connection or visit https://github.com/${REPO}/releases"
  fi

  echo "$version"
}

main() {
  info "DontGuess installer"
  printf "\n"

  check_deps

  OS=$(detect_os)
  ARCH=$(detect_arch)

  info "Detecting platform..."
  printf "  OS:   %s\n" "$OS"
  printf "  Arch: %s\n" "$ARCH"
  printf "\n"

  info "Finding latest release..."
  VERSION=$(get_latest_version)
  printf "  Version: %s\n" "$VERSION"
  printf "\n"

  LABEL="${OS}_${ARCH}"
  ARCHIVE="dontguess_${LABEL}.tar.gz"
  BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
  ARCHIVE_URL="${BASE_URL}/${ARCHIVE}"
  CHECKSUMS_URL="${BASE_URL}/checksums.txt"

  TMP_DIR=$(mktemp -d)
  trap 'rm -rf "$TMP_DIR"' EXIT

  info "Downloading..."
  printf "  %s\n" "$ARCHIVE_URL"

  if ! curl -fsSL --progress-bar -o "${TMP_DIR}/${ARCHIVE}" "$ARCHIVE_URL"; then
    die "Download failed. Check that version ${VERSION} has a release for ${LABEL}.\nVisit https://github.com/${REPO}/releases"
  fi

  printf "  checksums.txt\n"
  if ! curl -fsSL -o "${TMP_DIR}/checksums.txt" "$CHECKSUMS_URL"; then
    warn "Could not download checksums — skipping verification"
  else
    printf "\n"
    info "Verifying checksum..."
    EXPECTED=$(grep "${ARCHIVE}" "${TMP_DIR}/checksums.txt" | awk '{print $1}')
    if [ -z "$EXPECTED" ]; then
      warn "Checksum entry not found for ${ARCHIVE} — skipping verification"
    else
      ACTUAL=$(sha256_file "${TMP_DIR}/${ARCHIVE}")
      if [ -n "$ACTUAL" ] && [ "$ACTUAL" != "$EXPECTED" ]; then
        die "Checksum mismatch!\n  expected: ${EXPECTED}\n  got:      ${ACTUAL}"
      fi
      success "  Checksum OK"
    fi
  fi

  printf "\n"
  info "Extracting..."

  tar -xzf "${TMP_DIR}/${ARCHIVE}" -C "${TMP_DIR}"

  # Binaries are in dontguess_${LABEL}/ inside the archive
  BIN_DIR="${TMP_DIR}/dontguess_${LABEL}"

  DG_BIN="${BIN_DIR}/dontguess"
  SELLER_BIN="${BIN_DIR}/seller"

  if [ ! -f "$DG_BIN" ]; then
    die "dontguess binary not found in archive. Unexpected archive layout."
  fi

  printf "\n"
  info "Installing to ${INSTALL_DIR}..."
  mkdir -p "$INSTALL_DIR"

  cp "$DG_BIN" "${INSTALL_DIR}/dontguess"
  chmod +x "${INSTALL_DIR}/dontguess"
  success "  dontguess → ${INSTALL_DIR}/dontguess"

  if [ -f "$SELLER_BIN" ]; then
    cp "$SELLER_BIN" "${INSTALL_DIR}/seller"
    chmod +x "${INSTALL_DIR}/seller"
    success "  seller    → ${INSTALL_DIR}/seller"
  fi

  # PATH advice
  printf "\n"
  case ":${PATH}:" in
    *":${INSTALL_DIR}:"*)
      success "Done! ${INSTALL_DIR} is already in your PATH."
      ;;
    *)
      warn "${INSTALL_DIR} is not in your PATH."
      printf "\nAdd it:\n\n"
      printf "  ${BOLD}echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.profile && source ~/.profile${RESET}\n"
      ;;
  esac

  printf "\n"
  info "Next steps:"
  printf "\n"
  printf "  dontguess identity init        # create your agent identity\n"
  printf "  dontguess exchange join <url>   # join an exchange\n"
  printf "  dontguess put --help            # sell cached inference\n"
  printf "  dontguess buy --help            # buy cached inference\n"
  printf "\n"
  printf "  Docs: https://dontguess.ai\n"
  printf "\n"
}

main "$@"
