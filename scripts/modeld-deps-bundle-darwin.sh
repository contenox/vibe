#!/usr/bin/env bash
# Produce a native dependency bundle for modeld on macOS (darwin, .dylib).
#
# macOS = llama.cpp + Metal. OpenVINO GenAI is not supported on Apple Silicon, so the
# darwin bundle is llama-only (Metal GPU plugin + CPU); there is no OpenVINO in it.
# Peer of modeld-deps-bundle-linux.sh: same layout and manifest/fingerprint contract,
# minus OpenVINO. Run on a macOS device; push the result to S3.
#
# Inputs (env, set by the Makefile bundle target): PLATFORM OUT LLAMA_REF
# LLAMA_RUNTIME LLAMA_CPP_COMMIT  (OpenVINO inputs are ignored on darwin)
set -euo pipefail

fail() { echo "modeld-deps-bundle-darwin: $*" >&2; exit 1; }
need() { [ -n "${!1:-}" ] || fail "missing required env var: $1"; }
need_dir() { [ -d "$1" ] || fail "missing required directory: $1"; }
need_file() { [ -f "$1" ] || fail "missing required file: $1"; }

need PLATFORM; need OUT; need LLAMA_REF; need LLAMA_RUNTIME; need LLAMA_CPP_COMMIT
LLAMA_REF=${LLAMA_REF%/}; LLAMA_RUNTIME=${LLAMA_RUNTIME%/}; OUT=${OUT%/}

need_file "$LLAMA_REF/common/chat.h"
need_dir  "$LLAMA_REF/vendor"
need_dir  "$LLAMA_RUNTIME/include"
need_file "$LLAMA_RUNTIME/lib/libllama.dylib"
need_file "$LLAMA_RUNTIME/lib/libllama-common.dylib"

# The accelerator on macOS is Metal (no CUDA/HIP). The variant records metal vs a
# CPU-only fallback from the presence of the Metal ggml plugin. cuda/hip stay OFF in
# the fingerprint and the platform (darwin-*) already distinguishes the build.
build_type=Release; runtime_abi=dl-v1
STAMP="$LLAMA_RUNTIME/.contenox-runtime-stamp"
if [ -f "$STAMP" ]; then
  bt=$(sed -n 's/^build_type=//p' "$STAMP" | head -1); [ -n "$bt" ] && build_type=$bt
  ra=$(sed -n 's/^runtime_abi=//p' "$STAMP" | head -1); [ -n "$ra" ] && runtime_abi=$ra
fi
metal=false
ls "$LLAMA_RUNTIME"/lib/libggml-metal*.dylib >/dev/null 2>&1 && metal=true

# Fingerprint: darwin is always OpenVINO-free, so openvino=0 here matches what the
# fingerprint target / pull computes for a darwin platform.
FINGERPRINT=$(PLATFORM="$PLATFORM" LLAMA_CPP_COMMIT="$LLAMA_CPP_COMMIT" \
  LLAMA_BUILD_TYPE="$build_type" LLAMA_RUNTIME_ABI="$runtime_abi" \
  CUDA=OFF HIP=OFF OPENVINO=0 OPENVINO_GENAI_VERSION="" \
  bash "$(dirname -- "$0")/modeld-deps-fingerprint.sh")
variant=cpu; $metal && variant=metal

NAME="modeld-deps-${PLATFORM}-${variant}"
BUNDLE="$OUT/$NAME"
echo "modeld-deps-bundle-darwin: building $NAME (llama+metal=$metal, no openvino)"
rm -rf "$BUNDLE"
mkdir -p "$BUNDLE/llama/ref" "$BUNDLE/llama/runtime" "$BUNDLE/licenses/llama.cpp"

cp -a "$LLAMA_REF/common"  "$BUNDLE/llama/ref/common"
cp -a "$LLAMA_REF/vendor"  "$BUNDLE/llama/ref/vendor"
cp -a "$LLAMA_RUNTIME/include" "$BUNDLE/llama/runtime/include"
cp -a "$LLAMA_RUNTIME/lib"     "$BUNDLE/llama/runtime/lib"
[ -f "$STAMP" ] && cp -a "$STAMP" "$BUNDLE/llama/runtime/"
for lic in LICENSE LICENSE-* COPYING; do
  [ -f "$LLAMA_REF/$lic" ] && cp -a "$LLAMA_REF/$lic" "$BUNDLE/licenses/llama.cpp/" || true
done

cat > "$BUNDLE/manifest.json" <<EOF
{
  "platform": "$PLATFORM",
  "variant": "$variant",
  "fingerprint": "$FINGERPRINT",
  "llama_cpp_commit": "$LLAMA_CPP_COMMIT",
  "accelerator": { "cuda": false, "hip": false, "metal": $metal },
  "openvino": false,
  "libraries": [ "llama","ggml" ],
  "built_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF

cat > "$BUNDLE/bundle.env" <<EOF
MODELD_BUNDLE_PLATFORM=$PLATFORM
MODELD_BUNDLE_VARIANT=$variant
MODELD_BUNDLE_FINGERPRINT=$FINGERPRINT
MODELD_BUNDLE_OPENVINO=0
MODELD_BUNDLE_CUDA=0
MODELD_BUNDLE_HIP=0
MODELD_BUNDLE_METAL=$($metal && echo 1 || echo 0)
MODELD_BUNDLE_LLAMA_COMMIT=$LLAMA_CPP_COMMIT
EOF

echo "modeld-deps-bundle-darwin: bundle dir -> $BUNDLE"
echo "modeld-deps-bundle-darwin: fingerprint -> $FINGERPRINT"
echo "$NAME"
