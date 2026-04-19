#!/usr/bin/env bash
# Writes runtime/internal/web/beam/beam_ui_embed_stamp.txt with a hash of dist/ contents.
# The file is //go:embed'd next to dist/ so any UI rebuild changes package inputs
# and reliably invalidates the Go build (avoids stale embedded SPA in bin/contenox).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DIST="$ROOT/runtime/internal/web/beam/dist"
STAMP="$ROOT/runtime/internal/web/beam/beam_ui_embed_stamp.txt"
if [ ! -d "$DIST" ]; then
  echo "beam_embed_stamp.sh: missing $DIST (run vite build first)" >&2
  exit 1
fi
# Hash of sorted file hashes — stable, changes when any asset changes.
HASH=$(find "$DIST" -type f | LC_ALL=C sort | xargs sha256sum | sha256sum | awk '{print $1}')
printf '%s\n' "$HASH" >"$STAMP"
