#!/usr/bin/env bash
# verify_cli_help.sh — smoke test that the contenox binary exposes the expected
# top-level subcommands and exits cleanly. Invoked by `task test-cli-help`.
#
# Usage:
#   CONTENOX_BIN=./bin/contenox ./scripts/verify_cli_help.sh
#
# The Taskfile sets CONTENOX_BIN before calling this script.
set -euo pipefail

BIN="${CONTENOX_BIN:-./bin/contenox}"

if [[ ! -x "$BIN" ]]; then
  echo "ERROR: binary not found or not executable: $BIN" >&2
  exit 1
fi

echo "==> CLI help smoke: $BIN"

# 1. --help must exit 0.
HELP_OUTPUT="$("$BIN" --help 2>&1)"
echo "$HELP_OUTPUT" | head -5

# 2. Version string must be present.
if ! echo "$HELP_OUTPUT" | grep -q "Version:"; then
  echo "FAIL: 'Version:' not found in --help output" >&2
  exit 1
fi

# 3. Every top-level subcommand must appear in the help output. Keep this list
# in lockstep with the registrations in internal/surfaces/contenoxcli/cli.go — a command
# added there but not here is invisible to this gate, and vice versa.
EXPECTED_CMDS=(
  "acp"
  "acpx"
  "agent"
  "approvals"
  "backend"
  "new"
  "cache"
  "chat"
  "config"
  "doctor"
  "inbox"
  "index"
  "init"
  "mcp"
  "mission"
  "model"
  "resume"
  "run"
  "sandbox"
  "search"
  "session"
  "setup"
  "shell-env"
  "state"
  "tools"
  "update"
  "version"
  "workspace"
)

MISSING=()
for cmd in "${EXPECTED_CMDS[@]}"; do
  if ! echo "$HELP_OUTPUT" | grep -qE "^  $cmd[[:space:]]"; then
    MISSING+=("$cmd")
  fi
done

if [[ ${#MISSING[@]} -gt 0 ]]; then
  echo "FAIL: missing subcommand(s) in --help output: ${MISSING[*]}" >&2
  exit 1
fi

# 4. `contenox version` must exit 0 and print a version string.
VERSION_OUTPUT="$("$BIN" version 2>&1)"
if ! echo "$VERSION_OUTPUT" | grep -qE "v[0-9]+\.[0-9]+\.[0-9]+"; then
  echo "FAIL: 'contenox version' did not print a semver string" >&2
  echo "  Got: $VERSION_OUTPUT" >&2
  exit 1
fi

echo "==> OK: all ${#EXPECTED_CMDS[@]} subcommands present, version $VERSION_OUTPUT"
