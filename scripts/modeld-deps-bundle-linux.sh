#!/usr/bin/env bash
# Produce a native dependency bundle (a relocatable sysroot) for modeld.
#
# A bundle is a *build input* consumed by `make package-modeld-release` via
# MODELD_DEPS_ROOT. Bundles are built on whatever device/platform can compile the
# variant (CPU-only, CUDA, HIP, with or without OpenVINO) and pushed to S3; dev
# flows and release assembly download the right variant per platform and link
# modeld against it without rebuilding llama.cpp or OpenVINO.
#
# The layout faithfully relocates the dependency roots so the existing CGo flag
# expressions in mk/llama-flags.mk and mk/openvino-flags.mk resolve unchanged when
# package-modeld-release re-points the root variables at the bundle.
#
# Inputs are passed as environment variables (set by the Makefile bundle target):
#   PLATFORM                e.g. linux-amd64
#   OUT                     output directory for the bundle dir + archive
#   LLAMA_REF               pinned llama.cpp source checkout (common/, vendor/)
#   LLAMA_RUNTIME           built llama.cpp runtime (include/, lib/, stamp)
#   LLAMA_CPP_COMMIT        pinned llama.cpp commit
#   OPENVINO_PKG            OpenVINO runtime package dir (empty -> llama-only bundle)
#   GENAI_SRC               OpenVINO GenAI source checkout
#   GENAI_PKG               OpenVINO GenAI package dir (holds libopenvino_genai.so.*)
#   TOKENIZERS_LIB          OpenVINO tokenizers lib dir
#   OPENVINO_GENAI_VERSION  pinned OpenVINO GenAI version
set -euo pipefail

fail() { echo "modeld-deps-bundle: $*" >&2; exit 1; }
need() { [ -n "${!1:-}" ] || fail "missing required env var: $1"; }
need_dir() { [ -d "$1" ] || fail "missing required directory: $1"; }
need_file() { [ -f "$1" ] || fail "missing required file: $1"; }

need PLATFORM; need OUT; need LLAMA_REF; need LLAMA_RUNTIME; need LLAMA_CPP_COMMIT
LLAMA_REF=${LLAMA_REF%/}; LLAMA_RUNTIME=${LLAMA_RUNTIME%/}; OUT=${OUT%/}
OPENVINO_PKG=${OPENVINO_PKG:-}
GENAI_SRC=${GENAI_SRC:-}; GENAI_PKG=${GENAI_PKG:-}; TOKENIZERS_LIB=${TOKENIZERS_LIB:-}
OPENVINO_GENAI_VERSION=${OPENVINO_GENAI_VERSION:-}

# A bundle is OpenVINO-capable only if every OpenVINO input resolves; otherwise it
# is a llama-only bundle. The release decides per platform whether that is allowed.
HAVE_OPENVINO=0
if [ -n "$OPENVINO_PKG" ] && [ -d "$OPENVINO_PKG/include" ] && [ -d "$GENAI_SRC/src/cpp/include" ]; then
  HAVE_OPENVINO=1
fi

# Required llama.cpp inputs.
need_file "$LLAMA_REF/common/chat.h"
need_dir  "$LLAMA_REF/vendor"
need_dir  "$LLAMA_RUNTIME/include"
need_file "$LLAMA_RUNTIME/lib/libllama.so"
need_file "$LLAMA_RUNTIME/lib/libllama-common.so"

# Accelerator profile comes from what the llama runtime was actually built with,
# recorded in its build stamp. This is the bundle's variant axis: a device that can
# compile CUDA/HIP plugins produces a richer bundle than a CPU-only host.
STAMP="$LLAMA_RUNTIME/.contenox-runtime-stamp"
cuda=false; hip=false; cuda_raw=OFF; hip_raw=OFF
build_type=Release; runtime_abi=dl-v1
if [ -f "$STAMP" ]; then
  grep -qx 'cuda=ON' "$STAMP" && { cuda=true; cuda_raw=ON; } || true
  grep -qx 'hip=ON'  "$STAMP" && { hip=true; hip_raw=ON; } || true
  bt=$(sed -n 's/^build_type=//p' "$STAMP" | head -1); [ -n "$bt" ] && build_type=$bt
  ra=$(sed -n 's/^runtime_abi=//p' "$STAMP" | head -1); [ -n "$ra" ] && runtime_abi=$ra
fi

# Fingerprint of the build inputs (pins). Recorded in the manifest and used as the
# S3 key so we never rebuild/re-upload a version we already have.
FINGERPRINT=$(PLATFORM="$PLATFORM" LLAMA_CPP_COMMIT="$LLAMA_CPP_COMMIT" \
  LLAMA_BUILD_TYPE="$build_type" LLAMA_RUNTIME_ABI="$runtime_abi" \
  CUDA="$cuda_raw" HIP="$hip_raw" OPENVINO="$HAVE_OPENVINO" \
  OPENVINO_GENAI_VERSION="$OPENVINO_GENAI_VERSION" \
  bash "$(dirname -- "$0")/modeld-deps-fingerprint.sh")
variant=""
$cuda && variant="${variant:+$variant-}cuda"
$hip  && variant="${variant:+$variant-}hip"
[ -n "$variant" ] || variant="cpu"

NAME="modeld-deps-${PLATFORM}-${variant}"
BUNDLE="$OUT/$NAME"
echo "modeld-deps-bundle: building $NAME (openvino=$HAVE_OPENVINO cuda=$cuda hip=$hip)"
rm -rf "$BUNDLE"
mkdir -p "$BUNDLE/llama/ref" "$BUNDLE/llama/runtime" "$BUNDLE/licenses/llama.cpp"

# llama.cpp: the headers the build -I's, plus the runtime install (include + lib).
cp -a "$LLAMA_REF/common"  "$BUNDLE/llama/ref/common"
cp -a "$LLAMA_REF/vendor"  "$BUNDLE/llama/ref/vendor"
cp -a "$LLAMA_RUNTIME/include" "$BUNDLE/llama/runtime/include"
cp -a "$LLAMA_RUNTIME/lib"     "$BUNDLE/llama/runtime/lib"
[ -f "$STAMP" ] && cp -a "$STAMP" "$BUNDLE/llama/runtime/"

# CUDA hosts need libcudart/libcublas/libcublasLt to actually use the bundled
# libggml-cuda.so plugin — the NVIDIA driver package (what nvidia-smi needs)
# does not provide them, only a full CUDA Toolkit install does. Vendor them
# from this build host so the bundle is self-contained on a driver-only target.
$cuda && bash "$(dirname -- "$0")/modeld-vendor-cuda-libs.sh" "$BUNDLE/llama/runtime/lib"
for lic in LICENSE LICENSE-* COPYING; do
  [ -f "$LLAMA_REF/$lic" ] && cp -a "$LLAMA_REF/$lic" "$BUNDLE/licenses/llama.cpp/" || true
done

libraries='"llama","ggml"'
if [ "$HAVE_OPENVINO" = "1" ]; then
  need_dir "$GENAI_SRC/src/cpp/src"
  need_file "$GENAI_SRC/build/_deps/minja-src/include/minja/minja.hpp"
  need_file "$GENAI_SRC/build/_deps/gguflib-src/gguflib.h"
  need_dir "$OPENVINO_PKG/libs"
  need_dir "$TOKENIZERS_LIB"

  mkdir -p "$BUNDLE/openvino/openvino" \
           "$BUNDLE/openvino/genai/src/cpp" \
           "$BUNDLE/openvino/genai/build/_deps/minja-src" \
           "$BUNDLE/openvino/tokenizers/lib" \
           "$BUNDLE/licenses/openvino" "$BUNDLE/licenses/openvino-genai" "$BUNDLE/licenses/openvino-tokenizers"

  # OpenVINO runtime: headers + shared libs (libopenvino.so.* and device plugins).
  cp -a "$OPENVINO_PKG/include" "$BUNDLE/openvino/openvino/include"
  cp -a "$OPENVINO_PKG/libs"    "$BUNDLE/openvino/openvino/libs"

  # Populate license texts for OpenVINO components (Apache-2.0 requires LICENSE + NOTICE for redistribution).
  # This is critical for public releases (VS Code marketplace, ACP/Zed registries, Windows Store app).
  for srcdir in "$OPENVINO_PKG" "$GENAI_PKG" "$TOKENIZERS_LIB" "$GENAI_SRC"; do
    if [ -d "$srcdir" ]; then
      for f in LICENSE LICENSE.txt LICENSE.md NOTICE NOTICE.txt COPYING EULA EULA.txt; do
        if [ -f "$srcdir/$f" ]; then
          cp -a "$srcdir/$f" "$BUNDLE/licenses/openvino/" 2>/dev/null || true
          cp -a "$srcdir/$f" "$BUNDLE/licenses/openvino-genai/" 2>/dev/null || true
          cp -a "$srcdir/$f" "$BUNDLE/licenses/openvino-tokenizers/" 2>/dev/null || true
        fi
      done
    fi
  done

  # OpenVINO GenAI: the source the CGo bridge compiles against, the bundled minja /
  # gguflib headers, and the prebuilt libopenvino_genai.so.*.
  cp -a "$GENAI_SRC/src/cpp/include" "$BUNDLE/openvino/genai/src/cpp/include"
  cp -a "$GENAI_SRC/src/cpp/src"     "$BUNDLE/openvino/genai/src/cpp/src"
  cp -a "$GENAI_SRC/build/_deps/minja-src/include" "$BUNDLE/openvino/genai/build/_deps/minja-src/include"
  cp -a "$GENAI_SRC/build/_deps/gguflib-src"       "$BUNDLE/openvino/genai/build/_deps/gguflib-src"
  rm -rf "$BUNDLE/openvino/genai/build/_deps/gguflib-src/.git"
  genai_so=$(find "$GENAI_PKG" -maxdepth 1 -name 'libopenvino_genai.so*' | head -1)
  [ -n "$genai_so" ] || fail "no libopenvino_genai.so* in $GENAI_PKG"
  cp -a "$genai_so" "$BUNDLE/openvino/genai/"

  # OpenVINO tokenizers extension.
  cp -a "$TOKENIZERS_LIB/libopenvino_tokenizers.so" "$BUNDLE/openvino/tokenizers/lib/"

  libraries="$libraries,\"openvino\",\"openvino_genai\",\"openvino_tokenizers\""
fi

# manifest.json — the release-facing description verified before packaging.
cat > "$BUNDLE/manifest.json" <<EOF
{
  "platform": "$PLATFORM",
  "variant": "$variant",
  "fingerprint": "$FINGERPRINT",
  "llama_cpp_commit": "$LLAMA_CPP_COMMIT",
  "openvino_genai_version": "$OPENVINO_GENAI_VERSION",
  "accelerator": { "cuda": $cuda, "hip": $hip },
  "openvino": $([ "$HAVE_OPENVINO" = "1" ] && echo true || echo false),
  "libraries": [ $libraries ],
  "built_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF

# bundle.env — machine-readable companion that package-modeld-release sources, so
# the release path never has to parse JSON in shell to learn the expected backends.
cat > "$BUNDLE/bundle.env" <<EOF
MODELD_BUNDLE_PLATFORM=$PLATFORM
MODELD_BUNDLE_VARIANT=$variant
MODELD_BUNDLE_FINGERPRINT=$FINGERPRINT
MODELD_BUNDLE_OPENVINO=$HAVE_OPENVINO
MODELD_BUNDLE_CUDA=$($cuda && echo 1 || echo 0)
MODELD_BUNDLE_HIP=$($hip && echo 1 || echo 0)
MODELD_BUNDLE_LLAMA_COMMIT=$LLAMA_CPP_COMMIT
MODELD_BUNDLE_GENAI_VERSION=$OPENVINO_GENAI_VERSION
EOF

# Note: no archive. Dependency bundles are uploaded to S3 as plain files
# (aws s3 sync), so consumers download only the variant they need.
echo "modeld-deps-bundle: bundle dir -> $BUNDLE"
echo "modeld-deps-bundle: fingerprint -> $FINGERPRINT"
echo "$NAME"
