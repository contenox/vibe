#!/usr/bin/env sh
# install.sh — Contenox installer
# Usage:
#   curl -fsSL https://contenox.com/install.sh | sh
#   curl -fsSL https://contenox.com/install.sh | CONTENOX_WITH_MODELD=1 sh   # also preinstall the local inference daemon
set -e

REPO="contenox/beam"
BIN="contenox"

# ── Detect OS ─────────────────────────────────────────────────────────────────
OS="$(uname -s)"
case "${OS}" in
  Linux)  GOOS="linux" ;;
  Darwin) GOOS="darwin" ;;
  *)
    echo "Unsupported OS: ${OS}"
    echo "Please download manually from https://github.com/${REPO}/releases"
    exit 1
    ;;
esac

# ── Detect arch ───────────────────────────────────────────────────────────────
ARCH="$(uname -m)"
case "${ARCH}" in
  x86_64|amd64) GOARCH=amd64 ;;
  arm64|aarch64) GOARCH=arm64 ;;
  *)
    echo "Unsupported architecture: ${ARCH}"
    echo "Please download manually from https://github.com/${REPO}/releases"
    exit 1
    ;;
esac

# ── Fetch latest release tag ──────────────────────────────────────────────────
# Resolved from the releases/latest redirect (not the GitHub API, which is
# rate-limited for unauthenticated callers).
echo "Fetching latest Contenox release..."
LATEST_URL="https://github.com/${REPO}/releases/latest"
if command -v curl >/dev/null 2>&1; then
  TAG="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "${LATEST_URL}" | sed 's|.*/tag/||')"
elif command -v wget >/dev/null 2>&1; then
  TAG="$(wget --max-redirect=10 -qO /dev/null -S "${LATEST_URL}" 2>&1 | grep -i '^ *location:' | tail -1 | sed 's|.*/tag/||' | tr -d '\r')"
else
  echo "Error: curl or wget is required to install contenox."
  exit 1
fi

if [ -z "${TAG}" ]; then
  echo "Error: could not determine latest release tag."
  echo "Please download manually from https://github.com/${REPO}/releases"
  exit 1
fi

echo "Latest version: ${TAG}"

# ── Download binary ───────────────────────────────────────────────────────────
ASSET="contenox-${GOOS}-${GOARCH}"
URL="https://github.com/${REPO}/releases/download/${TAG}/${ASSET}"
TMP="$(mktemp)"

echo "Downloading ${ASSET}..."
if command -v curl >/dev/null 2>&1; then
  curl -fL --progress-bar --max-time 600 "${URL}" -o "${TMP}"
elif command -v wget >/dev/null 2>&1; then
  wget --show-progress -qO "${TMP}" "${URL}"
fi

chmod +x "${TMP}"

# ── macOS: strip quarantine flag (defensive; curl downloads usually don't get it) ──
if [ "${GOOS}" = "darwin" ]; then
  xattr -d com.apple.quarantine "${TMP}" 2>/dev/null || true
fi

# ── Install ────────────────────────────────────────────────────────────────────
EXISTING="$(command -v ${BIN} 2>/dev/null || true)"
if [ -n "${EXISTING}" ]; then
  INSTALL_DIR="$(dirname "${EXISTING}")"
else
  INSTALL_DIR="/usr/local/bin"
fi

if [ -w "${INSTALL_DIR}" ]; then
  mv "${TMP}" "${INSTALL_DIR}/${BIN}"
elif command -v sudo >/dev/null 2>&1; then
  echo "Moving to ${INSTALL_DIR} (sudo required)..."
  sudo mv "${TMP}" "${INSTALL_DIR}/${BIN}"
else
  INSTALL_DIR="${HOME}/.local/bin"
  mkdir -p "${INSTALL_DIR}"
  mv "${TMP}" "${INSTALL_DIR}/${BIN}"
  echo ""
  echo "Note: installed to ${INSTALL_DIR}/${BIN}"
  echo "Make sure ${INSTALL_DIR} is in your PATH."
fi

echo ""
echo "✓ contenox ${TAG} installed to ${INSTALL_DIR}/${BIN}"

# ── Optional: preinstall the local inference daemon (modeld) ──────────────────
# Local GGUF/OpenVINO inference needs the modeld daemon (~600 MB download).
# It also installs on demand when `contenox setup` selects a local provider.
if [ -n "${CONTENOX_WITH_MODELD}" ]; then
  echo ""
  echo "Installing the local modeld inference daemon (CONTENOX_WITH_MODELD set)..."
  "${INSTALL_DIR}/${BIN}" modeld install --backend "${CONTENOX_MODELD_BACKEND:-llama}"
fi

echo ""
echo "Get started:"
echo "  contenox setup                        # pick a provider/model (local models or a hosted API)"
echo "  contenox init                         # scaffold a workspace in your project directory"
echo "  contenox \"say hello world in python\"   # run a prompt"
echo "  contenox acp                          # speak ACP over stdio (Zed, JetBrains, AionUi)"
