package agenthost_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/services/agenthost"
	"github.com/contenox/libacp"
	"github.com/contenox/libacp/acpexec"
	"github.com/stretchr/testify/require"
)

// claudeACPBinEnv gates the one non-hermetic, nondeterministic agent in this
// suite; this suite asserts turn shape only and never runs in CI.
const claudeACPBinEnv = "ACP_CLAUDE_ACP_BIN"

// TestHostE2E_Claude_TurnShape pins turn shape (end_turn plus a reply chunk).
func TestHostE2E_Claude_TurnShape(t *testing.T) {
	requireSandboxable(t)
	bin := os.Getenv(claudeACPBinEnv)
	if bin == "" {
		t.Skipf("skipping: set %s to an executable serving Claude Code over ACP "+
			"(e.g. node_modules/.bin/claude-code-acp, or a wrapper script around "+
			"`npx -y @zed-industries/claude-code-acp`); requires Claude Code credentials "+
			"(a logged-in claude, or ANTHROPIC_API_KEY in the environment)", claudeACPBinEnv)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("%s=%q is not accessible: %v", claudeACPBinEnv, bin, err)
	}

	ctx, agent := registerAgent(t, "claude-smoke", bin)
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	var stderr acpexec.LockedBuffer
	harness := &agenthost.RecordingHarness{}
	res, err := agenthost.DriveTurn(ctx, agent, harness, agenthost.TurnRequest{
		Cwd:    t.TempDir(),
		Prompt: []libacp.ContentBlock{libacp.NewTextContent("This is an automated connection check. Reply with one short sentence.")},
		Stderr: &stderr,
		// The adapter never exits on stdin-close.
		KillGrace: 2 * time.Second,
	})
	require.NoError(t, err, "claude adapter stderr:\n%s", stderr.String())

	// Shape only: a live model's wording is not pinned.
	require.Equal(t, libacp.StopReasonEndTurn, res.StopReason)
	tracker := &libacp.TurnTracker{}
	for _, n := range harness.Updates() {
		tracker.Observe(n)
	}
	require.NoError(t, tracker.Err(res.StopReason), "claude adapter stderr:\n%s", stderr.String())

	t.Logf("claude replied (%d updates): %.200q", len(harness.Updates()), harness.MessageText())
}
