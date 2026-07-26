package agenthost_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/contenox/beam/internal/services/agenthost"
	"github.com/contenox/beam/libacp"
	"github.com/contenox/beam/libacp/acpexec"
	"github.com/stretchr/testify/require"
)

// claudeACPBinEnv gates the one non-hermetic server in this package's e2e
// suite: a real, foreign, production agent (Claude Code served over ACP, e.g.
// via the claude-code-acp adapter). Unlike stub/testy/loopback its replies
// are nondeterministic and it needs credentials, so this suite asserts turn
// SHAPE only and never runs in CI — it exists to prove the host drives a
// real-world agent through the exact same composed path, with zero
// Claude-specific code anywhere in the host.
const claudeACPBinEnv = "ACP_CLAUDE_ACP_BIN"

// TestHostE2E_Claude_TurnShape registers a Claude-over-ACP executable through
// the real registry leg and drives one live prompt turn through DriveTurn,
// asserting only what every well-behaved agent must produce: a normal
// end_turn and at least one displayable reply chunk.
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
		// The adapter is a persistent process that never exits on
		// stdin-close; don't wait out the full default grace on teardown.
		KillGrace: 2 * time.Second,
	})
	require.NoError(t, err, "claude adapter stderr:\n%s", stderr.String())

	// Shape only — never the reply text: a live model's wording is not ours
	// to pin. end_turn plus displayable output is the contract.
	require.Equal(t, libacp.StopReasonEndTurn, res.StopReason)
	tracker := &libacp.TurnTracker{}
	for _, n := range harness.Updates() {
		tracker.Observe(n)
	}
	require.NoError(t, tracker.Err(res.StopReason), "claude adapter stderr:\n%s", stderr.String())

	t.Logf("claude replied (%d updates): %.200q", len(harness.Updates()), harness.MessageText())
}
