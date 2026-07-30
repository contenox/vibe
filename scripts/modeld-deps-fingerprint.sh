#!/usr/bin/env bash
# Emit a deterministic fingerprint of the inputs a native dependency bundle is built
# from: the pinned source/version/accelerator profile. Two builds with the same
# fingerprint produce equivalent bundles, so a device can check S3 for the
# fingerprint and skip rebuilding/re-uploading a version we already have.
#
# The fingerprint is computed from identifiers only (no built artifacts), so it can
# be evaluated before the expensive runtime build. Both the bundle producer and the
# pre-build check call this one definition, so they can never drift.
#
# Inputs (env):
#   PLATFORM                 e.g. linux-amd64
#   LLAMA_CPP_COMMIT         pinned llama.cpp commit
#   LLAMA_BUILD_TYPE         e.g. Release
#   LLAMA_RUNTIME_ABI        e.g. dl-v1
#   CUDA                     ON | OFF
#   HIP                      ON | OFF
#   OPENVINO                 1 | 0
#   OPENVINO_GENAI_VERSION   pinned version (empty when OPENVINO=0)
set -euo pipefail

: "${PLATFORM:?missing PLATFORM}"
: "${LLAMA_CPP_COMMIT:?missing LLAMA_CPP_COMMIT}"
LLAMA_BUILD_TYPE=${LLAMA_BUILD_TYPE:-Release}
LLAMA_RUNTIME_ABI=${LLAMA_RUNTIME_ABI:-dl-v1}
CUDA=${CUDA:-OFF}
HIP=${HIP:-OFF}
OPENVINO=${OPENVINO:-0}
OPENVINO_GENAI_VERSION=${OPENVINO_GENAI_VERSION:-}
[ "$OPENVINO" = "1" ] || OPENVINO_GENAI_VERSION=""

# Stable, ordered canonical form. Do not reorder: the hash is a contract.
canonical=$(printf '%s\n' \
  "platform=$PLATFORM" \
  "llama_cpp_commit=$LLAMA_CPP_COMMIT" \
  "llama_build_type=$LLAMA_BUILD_TYPE" \
  "llama_runtime_abi=$LLAMA_RUNTIME_ABI" \
  "cuda=$CUDA" \
  "hip=$HIP" \
  "openvino=$OPENVINO" \
  "openvino_genai_version=$OPENVINO_GENAI_VERSION")

printf '%s' "$canonical" | sha256sum | cut -d' ' -f1
