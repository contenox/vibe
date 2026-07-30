#!/usr/bin/env bash
# Regenerate modeld/index.json from per-archive build metadata sidecars.
#
# Usage:
#   scripts/modeld-index-refresh.sh <MODELD_RELEASE_S3_URI-or-local-dir>
#
# The release prefix contains objects like:
#   vX.Y.Z/modeld-vX.Y.Z-linux-amd64.tar.gz.build.json
#
# Each sidecar is a single build object matching the index "builds" entry shape.
# This script downloads/reads all sidecars, writes index.json, then uploads it as
# the final release object.
set -euo pipefail

store=${1:?usage: modeld-index-refresh.sh <release-store-uri>}
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
store_tool="$root/scripts/modeld-store.sh"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

is_s3() { case "$1" in s3://*) return 0;; *) return 1;; esac; }

if is_s3 "$store"; then
  aws s3 sync "$store" "$tmp/store" --exclude "*" --include "*.build.json" >/dev/null
else
  mkdir -p "$tmp/store"
  if [ -d "$store" ]; then
    ( cd "$store" && find . -name '*.build.json' -type f -print0 ) | while IFS= read -r -d '' rel; do
      rel=${rel#./}
      mkdir -p "$tmp/store/$(dirname -- "$rel")"
      cp -a "$store/$rel" "$tmp/store/$rel"
    done
  fi
fi

mapfile -t files < <(find "$tmp/store" -name '*.build.json' -type f | sort)
[ "${#files[@]}" -gt 0 ] || { echo "modeld-index-refresh: no *.build.json files found under $store" >&2; exit 1; }

index="$tmp/index.json"
{
  printf '{\n  "schema": 1,\n  "builds": [\n'
  first=1
  for f in "${files[@]}"; do
    if [ "$first" = 0 ]; then printf ',\n'; fi
    sed 's/^/    /' "$f"
    first=0
  done
  printf '\n  ]\n}\n'
} > "$index"

"$store_tool" cp "$index" "$store/index.json"
echo "modeld-index-refresh: wrote $store/index.json from ${#files[@]} build metadata file(s)"
