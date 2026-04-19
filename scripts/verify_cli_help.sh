#!/usr/bin/env bash
# Smoke-test that the contenox binary starts and every documented top-level command prints help.
# Keeps CLI docs and Cobra wiring in sync (run after: make build-contenox).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${CONTENOX_BIN:-$ROOT/bin/contenox}"
if [[ ! -x "$BIN" ]]; then
  echo "error: missing executable: $BIN (run: make build-contenox)" >&2
  exit 1
fi

run_help() {
  local args=("$@")
  echo "  $BIN ${args[*]} --help"
  "$BIN" "${args[@]}" --help >/dev/null
}

run_help
run_help run
run_help chat
run_help plan
run_help plan next
run_help session
run_help init
run_help beam
run_help hook
run_help mcp
run_help backend
run_help config
run_help model
run_help doctor
run_help completion

echo "OK: all help pages succeeded ($BIN)"
