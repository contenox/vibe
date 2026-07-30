#!/usr/bin/env bash
# Finalize a packaged release modeld bundle: smoke-test the backend set, write the
# package manifest, and produce the archive + checksum.
#
# The smoke gate is the whole point of a separate release path: it runs
# `modeld version --json` against the freshly packaged binary and asserts the
# compiled-in backends match what the release expects, so a build can never silently
# ship fewer backends than intended.
#
# Inputs (env, set by the Makefile package-modeld-release target):
#   DIST_DIR         the packaged bundle dir (contains modeld wrapper + modeld.bin)
#   RELEASE_OUT      directory to write the archive + checksum into
#   NAME             archive base name, e.g. modeld-v0.32.5-linux-amd64
#   VERSION          expected modeld version, e.g. v0.32.5
#   PLATFORM         e.g. linux-amd64
#   EXPECT_OPENVINO  1 to require the openvino backend, 0 for llama-only
#   MIN_PROTOCOL     oldest supported transport protocol for this runtime
#   PROTOCOL_VERSION newest supported transport protocol for this runtime
set -euo pipefail

fail() { echo "modeld-package-release: $*" >&2; exit 1; }
for v in DIST_DIR RELEASE_OUT NAME VERSION PLATFORM EXPECT_OPENVINO; do
  [ -n "${!v:-}" ] || fail "missing required env var: $v"
done
LAUNCHER=${LAUNCHER:-modeld}
TARGET_OS=${TARGET_OS:-linux}
[ -f "$DIST_DIR/$LAUNCHER" ] || fail "missing packaged launcher: $DIST_DIR/$LAUNCHER"
if [ "$TARGET_OS" != "windows" ]; then
  [ -x "$DIST_DIR/$LAUNCHER" ] || fail "packaged launcher is not executable: $DIST_DIR/$LAUNCHER"
fi

if [ "${MODELD_PACKAGE_DEFER_SMOKE:-0}" = "1" ]; then
  echo "modeld-package-release: deferred smoke/finalize for $DIST_DIR/$LAUNCHER"
  exit 0
fi

# Smoke gate: the packaged binary must run and report the expected backends. The
# wrapper/launcher resolves the bundled native libs, so this also proves the link is sound.
if [ -n "${MODELD_PACKAGE_REPORT_FILE:-}" ]; then
  [ -f "$MODELD_PACKAGE_REPORT_FILE" ] || fail "missing report file: $MODELD_PACKAGE_REPORT_FILE"
  echo "modeld-package-release: smoke report <- $MODELD_PACKAGE_REPORT_FILE"
  report=$(cat "$MODELD_PACKAGE_REPORT_FILE")
else
  echo "modeld-package-release: smoke -> $DIST_DIR/$LAUNCHER version --json"
  report=$("$DIST_DIR/$LAUNCHER" version --json) || fail "packaged binary failed to run 'version'"
fi
echo "$report"

reported_version=$(printf '%s' "$report" | sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' | head -1)
[ "$reported_version" = "$VERSION" ] || fail "version mismatch: binary reports '$reported_version', expected '$VERSION'"

reported_protocol=$(printf '%s' "$report" | sed -n 's/.*"protocol": *\([0-9][0-9]*\).*/\1/p' | head -1)
[ -n "$reported_protocol" ] || fail "packaged binary did not report transport protocol"
[ "$reported_protocol" -gt 0 ] || fail "packaged binary reported invalid protocol: $reported_protocol"
if [ -n "${MIN_PROTOCOL:-}" ] && [ "$reported_protocol" -lt "$MIN_PROTOCOL" ]; then
  fail "protocol mismatch: binary reports $reported_protocol, minimum supported is $MIN_PROTOCOL"
fi
if [ -n "${PROTOCOL_VERSION:-}" ] && [ "$reported_protocol" -gt "$PROTOCOL_VERSION" ]; then
  fail "protocol mismatch: binary reports $reported_protocol, maximum supported is $PROTOCOL_VERSION"
fi

have_backend() { printf '%s' "$report" | grep -q "\"$1\""; }
have_backend llama || fail "packaged binary does not report the 'llama' backend"
if [ "$EXPECT_OPENVINO" = "1" ]; then
  have_backend openvino || fail "EXPECT_OPENVINO=1 but packaged binary does not report the 'openvino' backend (release refuses to ship a reduced backend set)"
fi

llama_commit=$(printf '%s' "$report" | sed -n 's/.*"llama_cpp_commit": *"\([^"]*\)".*/\1/p' | head -1)
genai_version=$(printf '%s' "$report" | sed -n 's/.*"openvino_genai_version": *"\([^"]*\)".*/\1/p' | head -1)

backends_json=$(printf '%s' "$report" | tr -d '\n' | sed -n 's/.*"backends": *\(\[[^]]*\]\).*/\1/p' | tr -s ' ')
[ -n "$backends_json" ] || backends_json="[]"

cat > "$DIST_DIR/manifest.json" <<EOF
{
  "modeld_version": "$VERSION",
  "protocol": $reported_protocol,
  "platform": "$PLATFORM",
  "backends": $backends_json,
  "llama_cpp_commit": "$llama_commit",
  "openvino_genai_version": "$genai_version",
  "openvino": $([ "$EXPECT_OPENVINO" = "1" ] && echo true || echo false),
  "built_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF

mkdir -p "$RELEASE_OUT"
parent=$(CDPATH= cd -- "$(dirname -- "$DIST_DIR")" && pwd)
base=$(basename -- "$DIST_DIR")
# Windows ships a .zip; other platforms a .tar.gz.
if [ "$TARGET_OS" = "windows" ]; then
  archive="$RELEASE_OUT/$NAME.zip"
  ( cd "$parent" && zip -qr "$archive" "$base" )
else
  archive="$RELEASE_OUT/$NAME.tar.gz"
  tar -czf "$archive" -C "$parent" "$base"
fi
arcbase=$(basename -- "$archive")
( cd "$RELEASE_OUT" && sha256sum "$arcbase" > "$arcbase.sha256" )
size=$(wc -c < "$archive" | tr -d ' ')
channel=${MODELD_RELEASE_CHANNEL:-stable}
cat > "$archive.build.json" <<EOF
{
  "version": "$VERSION",
  "platform": "$PLATFORM",
  "protocol": $reported_protocol,
  "backends": $backends_json,
  "channel": "$channel",
  "archive": "$VERSION/$arcbase",
  "sha256": "$VERSION/$arcbase.sha256",
  "size": $size
}
EOF
echo "modeld-package-release: archive -> $archive"
echo "modeld-package-release: checksum -> $RELEASE_OUT/$arcbase.sha256"
echo "modeld-package-release: build metadata -> $archive.build.json"
