#!/usr/bin/env bash
# Produce a native dependency bundle for modeld on Windows (.dll + import libs).
#
# Peer of modeld-deps-bundle-linux.sh; same bundle layout and manifest/fingerprint
# contract, with Windows library names. Run under a bash shell on a Windows build
# host; the runtime stamp carries the actual native ABI/toolchain identity (for
# example dl-v1-msvc for the Clang/MSVC ABI build).
#
# Inputs (env, set by the Makefile bundle target): PLATFORM OUT LLAMA_REF
# LLAMA_RUNTIME LLAMA_CPP_COMMIT OPENVINO_PKG GENAI_SRC GENAI_PKG TOKENIZERS_LIB
# OPENVINO_GENAI_VERSION
set -euo pipefail

fail() { echo "modeld-deps-bundle-windows: $*" >&2; exit 1; }
need() { [ -n "${!1:-}" ] || fail "missing required env var: $1"; }
need_dir() { [ -d "$1" ] || fail "missing required directory: $1"; }
need_file() { [ -f "$1" ] || fail "missing required file: $1"; }
glob1() { find "$1" -maxdepth 1 \( "${@:2}" \) 2>/dev/null | head -1; }

need PLATFORM; need OUT; need LLAMA_REF; need LLAMA_RUNTIME; need LLAMA_CPP_COMMIT
LLAMA_REF=${LLAMA_REF%/}; LLAMA_RUNTIME=${LLAMA_RUNTIME%/}; OUT=${OUT%/}
OPENVINO_PKG=${OPENVINO_PKG:-}
GENAI_SRC=${GENAI_SRC:-}; GENAI_PKG=${GENAI_PKG:-}; TOKENIZERS_LIB=${TOKENIZERS_LIB:-}
OPENVINO_GENAI_VERSION=${OPENVINO_GENAI_VERSION:-}

HAVE_OPENVINO=0
if [ -n "$OPENVINO_PKG" ] && [ -d "$OPENVINO_PKG/include" ] && [ -d "$GENAI_SRC/src/cpp/include" ]; then
  HAVE_OPENVINO=1
fi

need_file "$LLAMA_REF/common/chat.h"
need_dir  "$LLAMA_REF/vendor"
need_dir  "$LLAMA_RUNTIME/include"
common_dll=$(glob1 "$LLAMA_RUNTIME/lib" -name 'libllama-common.dll' -o -name 'llama-common.dll' -o -name 'common.dll')
[ -n "$common_dll" ] || fail "no llama common .dll in $LLAMA_RUNTIME/lib"
llama_dll=$(glob1 "$LLAMA_RUNTIME/lib" -name 'libllama.dll' -o -name 'llama.dll')
[ -n "$llama_dll" ] || fail "no llama .dll in $LLAMA_RUNTIME/lib"

# Accelerator from the llama runtime stamp (Windows can build CUDA; HIP is unusual).
STAMP="$LLAMA_RUNTIME/.contenox-runtime-stamp"
cuda=false; hip=false; cuda_raw=OFF; hip_raw=OFF
build_type=Release; runtime_abi=dl-v1
if [ -f "$STAMP" ]; then
  grep -qx 'cuda=ON' "$STAMP" && { cuda=true; cuda_raw=ON; } || true
  grep -qx 'hip=ON'  "$STAMP" && { hip=true; hip_raw=ON; } || true
  bt=$(sed -n 's/^build_type=//p' "$STAMP" | head -1); [ -n "$bt" ] && build_type=$bt
  ra=$(sed -n 's/^runtime_abi=//p' "$STAMP" | head -1); [ -n "$ra" ] && runtime_abi=$ra
fi

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
echo "modeld-deps-bundle-windows: building $NAME (openvino=$HAVE_OPENVINO cuda=$cuda hip=$hip)"
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

  cp -a "$GENAI_SRC/src/cpp/include" "$BUNDLE/openvino/genai/src/cpp/include"
  cp -a "$GENAI_SRC/src/cpp/src"     "$BUNDLE/openvino/genai/src/cpp/src"
  cp -a "$GENAI_SRC/build/_deps/minja-src/include" "$BUNDLE/openvino/genai/build/_deps/minja-src/include"
  cp -a "$GENAI_SRC/build/_deps/gguflib-src"       "$BUNDLE/openvino/genai/build/_deps/gguflib-src"
  rm -rf "$BUNDLE/openvino/genai/build/_deps/gguflib-src/.git"
  genai_so=$(glob1 "$GENAI_PKG" -name 'libopenvino_genai*.dll' -o -name 'openvino_genai*.dll')
  [ -n "$genai_so" ] || fail "no openvino_genai*.dll in $GENAI_PKG"
  cp -a "$genai_so" "$BUNDLE/openvino/genai/"
  genai_lib=$(glob1 "$GENAI_PKG" -name 'libopenvino_genai*.lib' -o -name 'openvino_genai*.lib')
  [ -z "$genai_lib" ] || cp -a "$genai_lib" "$BUNDLE/openvino/genai/"

  tok=$(glob1 "$TOKENIZERS_LIB" -name 'libopenvino_tokenizers.dll' -o -name 'openvino_tokenizers.dll')
  [ -n "$tok" ] || fail "no openvino_tokenizers.dll in $TOKENIZERS_LIB"
  cp -a "$tok" "$BUNDLE/openvino/tokenizers/lib/"

  libraries="$libraries,\"openvino\",\"openvino_genai\",\"openvino_tokenizers\""
fi

cat > "$BUNDLE/manifest.json" <<EOF
{
  "platform": "$PLATFORM",
  "variant": "$variant",
  "fingerprint": "$FINGERPRINT",
  "llama_cpp_commit": "$LLAMA_CPP_COMMIT",
  "llama_runtime_abi": "$runtime_abi",
  "openvino_genai_version": "$OPENVINO_GENAI_VERSION",
  "accelerator": { "cuda": $cuda, "hip": $hip },
  "openvino": $([ "$HAVE_OPENVINO" = "1" ] && echo true || echo false),
  "libraries": [ $libraries ],
  "built_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF

cat > "$BUNDLE/bundle.env" <<EOF
MODELD_BUNDLE_PLATFORM=$PLATFORM
MODELD_BUNDLE_VARIANT=$variant
MODELD_BUNDLE_FINGERPRINT=$FINGERPRINT
MODELD_BUNDLE_RUNTIME_ABI=$runtime_abi
MODELD_BUNDLE_OPENVINO=$HAVE_OPENVINO
MODELD_BUNDLE_CUDA=$($cuda && echo 1 || echo 0)
MODELD_BUNDLE_HIP=$($hip && echo 1 || echo 0)
MODELD_BUNDLE_LLAMA_COMMIT=$LLAMA_CPP_COMMIT
MODELD_BUNDLE_GENAI_VERSION=$OPENVINO_GENAI_VERSION
EOF

echo "modeld-deps-bundle-windows: bundle dir -> $BUNDLE"
echo "modeld-deps-bundle-windows: fingerprint -> $FINGERPRINT"
echo "$NAME"
