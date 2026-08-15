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

# The opt-in-beta gate is pinned per pass rather than inherited: an operator's
# real ~/.contenox has `opt-in-beta true`, CI's HOME is empty, and that
# divergence once let a beta-hidden command pass locally and fail in CI. Both
# states are asserted below, so neither environment can hide the other's bug.

# 1. --help must exit 0.
HELP_OUTPUT="$(CONTENOX_OPT_IN_BETA=false "$BIN" --help 2>&1)"
BETA_HELP_OUTPUT="$(CONTENOX_OPT_IN_BETA=true "$BIN" --help 2>&1)"
echo "$HELP_OUTPUT" | head -5

# 2. Version string must be present.
if ! echo "$HELP_OUTPUT" | grep -q "Version:"; then
  echo "FAIL: 'Version:' not found in --help output" >&2
  exit 1
fi

# 3. Every top-level subcommand must appear in the help output. Keep this list
# in lockstep with the registrations in internal/surfaces/contenoxcli/cli.go — a command
# added there but not here is invisible to this gate, and vice versa.
STABLE_CMDS=(
  "acp"
  "acpx"
  "agent"
  "approvals"
  "autocomplete"
  "backend"
  "cache"
  "chat"
  "config"
  "doctor"
  "hitl"
  "inbox"
  "index"
  "init"
  "mcp"
  "mission"
  "model"
  "pair"
  "run"
  "sandbox"
  "search"
  "serve"
  "session"
  "setup"
  "shell-env"
  "state"
  "tools"
  "unpair"
  "update"
  "version"
  "vet"
  "workspace"
)

# BETA_CMDS are registered unconditionally but marked Hidden without the
# opt-in (see the betaHidden block in cli.go). They are asserted absent from
# the stable help and present under the opt-in, so the gate still fails if one
# is deleted outright rather than merely hidden.
BETA_CMDS=(
  "events"
)

# has_cmd reports whether $2 lists $1 as a top-level subcommand.
has_cmd() {
  echo "$2" | grep -qE "^  $1[[:space:]]"
}

MISSING=()
for cmd in "${STABLE_CMDS[@]}"; do
  has_cmd "$cmd" "$HELP_OUTPUT" || MISSING+=("$cmd")
done

if [[ ${#MISSING[@]} -gt 0 ]]; then
  echo "FAIL: missing subcommand(s) in --help output: ${MISSING[*]}" >&2
  exit 1
fi

# Stable commands must not vanish when beta is enabled either: the opt-in adds
# surface, it never replaces it.
MISSING=()
for cmd in "${STABLE_CMDS[@]}" "${BETA_CMDS[@]}"; do
  has_cmd "$cmd" "$BETA_HELP_OUTPUT" || MISSING+=("$cmd")
done

if [[ ${#MISSING[@]} -gt 0 ]]; then
  echo "FAIL: missing subcommand(s) in --help output under opt-in-beta: ${MISSING[*]}" >&2
  exit 1
fi

LEAKED=()
for cmd in "${BETA_CMDS[@]}"; do
  has_cmd "$cmd" "$HELP_OUTPUT" && LEAKED+=("$cmd")
done

if [[ ${#LEAKED[@]} -gt 0 ]]; then
  echo "FAIL: beta subcommand(s) visible without opt-in-beta: ${LEAKED[*]}" >&2
  exit 1
fi

# 4. `contenox version` must exit 0 and print a version string.
VERSION_OUTPUT="$("$BIN" version 2>&1)"
if ! echo "$VERSION_OUTPUT" | grep -qE "v[0-9]+\.[0-9]+\.[0-9]+"; then
  echo "FAIL: 'contenox version' did not print a semver string" >&2
  echo "  Got: $VERSION_OUTPUT" >&2
  exit 1
fi

echo "==> OK: ${#STABLE_CMDS[@]} stable + ${#BETA_CMDS[@]} beta-gated subcommands present, version $VERSION_OUTPUT"
