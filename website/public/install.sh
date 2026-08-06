#!/usr/bin/env sh
# install.sh — Contenox installer
# Usage:
#   curl -fsSL https://contenox.com/install.sh | sh
set -e

REPO="contenox/contenox"
BIN="contenox"

TMP=""
SUMS=""
cleanup() {
  [ -n "${TMP}" ] && rm -f "${TMP}"
  [ -n "${SUMS}" ] && rm -f "${SUMS}"
  return 0
}
trap cleanup EXIT INT TERM

# fetch <url> <dest> [--quiet] — download or return non-zero.
fetch() {
  _url="$1"
  _dest="$2"
  _quiet="${3:-}"
  if command -v curl >/dev/null 2>&1; then
    if [ -n "${_quiet}" ]; then
      curl -fsSL --max-time 600 "${_url}" -o "${_dest}"
    else
      curl -fL --progress-bar --max-time 600 "${_url}" -o "${_dest}"
    fi
  elif command -v wget >/dev/null 2>&1; then
    if [ -n "${_quiet}" ]; then
      wget -qO "${_dest}" "${_url}"
    else
      wget --show-progress -qO "${_dest}" "${_url}"
    fi
  else
    echo "Error: curl or wget is required to install contenox."
    exit 1
  fi
}

# sha256_of <file> — print the lowercase hex digest, or exit if no tool exists.
# sha256sum is coreutils (Linux); macOS ships shasum and openssl instead.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "$1" | awk '{print $NF}'
  else
    echo "Error: no SHA-256 tool found (need sha256sum, shasum, or openssl)." >&2
    echo "Refusing to install an unverified binary." >&2
    exit 1
  fi
}

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
fetch "${URL}" "${TMP}"

# ── Verify against the release checksum manifest ──────────────────────────────
# Fails closed: no manifest, no entry, or a mismatch aborts the install. The
# binary is never marked executable or moved into PATH before this passes.
SUMS_URL="https://github.com/${REPO}/releases/download/${TAG}/SHA256SUMS"
SUMS="$(mktemp)"

echo "Verifying checksum..."
if ! fetch "${SUMS_URL}" "${SUMS}" --quiet; then
  echo ""
  echo "Error: could not download SHA256SUMS for ${TAG}."
  echo "Refusing to install an unverified binary."
  echo "  expected: ${SUMS_URL}"
  echo "Releases before checksums were published do not carry this file."
  echo "Install a newer release, or download and verify manually from"
  echo "  https://github.com/${REPO}/releases"
  exit 1
fi

EXPECTED="$(awk -v want="${ASSET}" '$2 == want || $2 == "*" want {print $1; exit}' "${SUMS}")"
if [ -z "${EXPECTED}" ]; then
  echo ""
  echo "Error: SHA256SUMS for ${TAG} has no entry for ${ASSET}."
  echo "Refusing to install an unverified binary."
  exit 1
fi

ACTUAL="$(sha256_of "${TMP}" | tr '[:upper:]' '[:lower:]')"
EXPECTED="$(echo "${EXPECTED}" | tr '[:upper:]' '[:lower:]')"

if [ "${ACTUAL}" != "${EXPECTED}" ]; then
  echo ""
  echo "Error: CHECKSUM MISMATCH for ${ASSET}."
  echo "  expected: ${EXPECTED}"
  echo "  actual:   ${ACTUAL}"
  echo ""
  echo "The downloaded file does not match the published release. This could be"
  echo "a corrupted download or a tampered artifact. Nothing was installed."
  echo "Report it at https://github.com/${REPO}/security/advisories"
  exit 1
fi

echo "✓ checksum verified (sha256:${ACTUAL})"

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

echo ""
echo "Get started:"
echo "  contenox setup                        # pick a provider/model (local models or a hosted API)"
echo "  contenox init                         # scaffold a workspace in your project directory"
echo "  contenox \"say hello world in python\"   # run a prompt"
echo "  contenox acp                          # speak ACP over stdio (Zed, JetBrains, AionUi)"
