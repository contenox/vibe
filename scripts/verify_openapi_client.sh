#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SPEC_PATH="${OPENAPI_SPEC:-${1:-$PROJECT_ROOT/docs/openapi.json}}"
TMP_DIR="$(mktemp -d)"
OUT_DIR="$TMP_DIR/client"

cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

if [[ ! -f "$SPEC_PATH" ]]; then
  echo "OpenAPI spec not found: $SPEC_PATH" >&2
  exit 1
fi

cd "$PROJECT_ROOT"

npm exec -- openapi --input "$SPEC_PATH" --output "$OUT_DIR" --client fetch >/dev/null

test -f "$OUT_DIR/index.ts"
test -f "$OUT_DIR/core/OpenAPI.ts"
test -d "$OUT_DIR/models"

echo "OpenAPI client generation smoke test passed."
