#!/usr/bin/env bash
# Vendors the CUDA Toolkit runtime shared libraries that libggml-cuda.so
# dynamically links against (libcudart, libcublas, libcublasLt) into a modeld
# lib directory, so a relocatable modeld package works on a target machine
# that has only the NVIDIA driver (what `nvidia-smi` needs) and not a full
# CUDA Toolkit install.
#
# Without this, GGML_BACKEND_DL's runtime dlopen of libggml-cuda.so silently
# fails on such a host (its own dependencies are unresolved) and modeld
# correctly, silently falls back to CPU — the accelerator probe isn't wrong,
# the bundle was just never actually self-contained for CUDA.
#
# Usage: modeld-vendor-cuda-libs.sh <lib-dir-containing-libggml-cuda.so>
set -euo pipefail

LIB_DIR=${1:?usage: modeld-vendor-cuda-libs.sh <lib-dir>}
PLUGIN="$LIB_DIR/libggml-cuda.so"

if [ ! -f "$PLUGIN" ]; then
  echo "modeld-vendor-cuda-libs: no libggml-cuda.so in $LIB_DIR, nothing to vendor"
  exit 0
fi

# The CUDA Toolkit's own runtime/BLAS libraries — everything else libggml-cuda.so
# needs (libc, libstdc++, libgcc_s, libpthread, libdl, librt, libm) is base-OS and
# already on any Linux target; libcuda.so.1 comes from the NVIDIA driver package,
# which a GPU host already has if `nvidia-smi` works there.
WANTED_RE='^lib(cudart|cublas|cublasLt)\.so\.[0-9]+$'

found=0
while IFS= read -r line; do
  name=$(awk '{print $1}' <<<"$line")
  path=$(awk '{print $3}' <<<"$line")
  [[ "$name" =~ $WANTED_RE ]] || continue
  if [ -z "$path" ] || [ ! -f "$path" ]; then
    echo "modeld-vendor-cuda-libs: cannot resolve $name (this build host is missing CUDA Toolkit runtime libs?)" >&2
    exit 1
  fi
  cp -L "$path" "$LIB_DIR/$name"
  echo "modeld-vendor-cuda-libs: vendored $name -> $LIB_DIR/$name"
  found=$((found + 1))
done < <(ldd "$PLUGIN")

if [ "$found" -lt 3 ]; then
  echo "modeld-vendor-cuda-libs: expected libcudart/libcublas/libcublasLt, only vendored $found" >&2
  exit 1
fi

# Also stage NVIDIA CUDA EULA / license notice if discoverable near the toolkit.
# Required for compliant redistribution of CUDA runtime libs in public modeld artifacts.
CUDA_EULA_CANDIDATES=(
  "${CUDA_HOME:-}/EULA.txt"
  "${CUDA_PATH:-}/EULA.txt"
  "/usr/local/cuda/EULA.txt"
  "$(dirname "$(which nvcc 2>/dev/null || echo '')")/../../EULA.txt"
)
for cand in "${CUDA_EULA_CANDIDATES[@]}"; do
  if [ -f "$cand" ]; then
    mkdir -p "$LIB_DIR/../licenses/cuda"
    cp -a "$cand" "$LIB_DIR/../licenses/cuda/" 2>/dev/null || cp -a "$cand" "$LIB_DIR/licenses/cuda/" 2>/dev/null || true
    echo "modeld-vendor-cuda-libs: staged NVIDIA EULA from $cand"
    break
  fi
done
# On Windows the EULA is usually in the NVIDIA GPU Computing Toolkit dir; user/build host should ensure it lands in licenses/ via manual or extended logic.
