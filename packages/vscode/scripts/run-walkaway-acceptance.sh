#!/usr/bin/env bash
# THE acceptance test for Phase 5 "walk-away"
# (docs/development/internal/vscode-implementation-plan.md §5):
#   1. Prompt something that trips a gated tool call.
#   2. Let it park / suspend.
#   3. Close VS Code entirely.
#   4. Answer from the CLI: ./bin/contenox approvals respond ...
#   5. Reopen VS Code.
#   6. Assert the run completed and the result is visible in the transcript.
#
# Each phase is a SEPARATE `vscode-test` process launch (real close, real
# reopen -- not a mock of either), with a real CLI verdict shelled out in
# between. Run from packages/vscode. Requires `npm run build` to have
# produced bin/contenox already (this script does not build it).
set -euo pipefail
cd "$(dirname "$0")/.."

BIN="./bin/contenox"
if [ ! -x "$BIN" ]; then
  echo "missing $BIN -- run 'npm run build' first" >&2
  exit 1
fi

XVFB=""
if command -v xvfb-run >/dev/null 2>&1; then
  XVFB="xvfb-run -a"
fi

echo "=== Phase A: park (steps 1-2) ==="
CONTENOX_WALKAWAY_PHASE=park $XVFB npm run test:walkaway
echo "vscode-test exited: VS Code process is gone (step 3, 'close VS Code entirely')."

STATE_FILE=".vscode-test/walkaway-state.json"
if [ ! -f "$STATE_FILE" ]; then
  echo "no $STATE_FILE written by the park phase -- it must have failed before reaching suspension" >&2
  exit 1
fi
APPROVAL_ID="$(node -e "console.log(require('./$STATE_FILE').approvalId)")"
SESSION_ID="$(node -e "console.log(require('./$STATE_FILE').sessionId)")"
echo "parked approval id: $APPROVAL_ID (session $SESSION_ID)"

echo "=== Step 4: answer from the CLI, VS Code fully closed ==="
"$BIN" approvals list
"$BIN" approvals respond "$APPROVAL_ID" --approve

echo "=== Phase B: verify (steps 5-6) ==="
CONTENOX_WALKAWAY_PHASE=verify $XVFB npm run test:walkaway

echo "=== Walk-away acceptance test passed ==="
"$BIN" session list
