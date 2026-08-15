package contenoxcli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/contenox/contenox/libtracker"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

const reviewerDeclaration = `---
name: reviewer
description: Reviews a file for correctness problems
tools: Read, Grep
---

You review code.
`

// The whole front door in one test: a Markdown file in .contenox/agents/
// becomes a registered agent under the name its frontmatter gave it. Nothing
// an operator types carries the word chain.
func TestUnit_DeclaredAgentRegistersUnderItsBareName(t *testing.T) {
	contenoxDir := t.TempDir()
	agentsDir := filepath.Join(contenoxDir, agentdecl.NativeSourceDir)
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(agentsDir, "reviewer.md"), []byte(reviewerDeclaration), 0o644))

	dbPath := filepath.Join(t.TempDir(), "agents.db")
	ctx, svc, done := openServiceAt(t, dbPath)
	defer done()

	discoverChainAgents(ctx, svc, contenoxDir, libtracker.NoopTracker{}, DiscoverDeps{})

	agent, err := svc.GetByName(ctx, "reviewer")
	require.NoError(t, err, "a declaration named reviewer must register as reviewer")
	require.True(t, agent.Enabled)

	_, err = svc.GetByName(ctx, "chain-reviewer")
	require.Error(t, err, "the chain- prefix must not survive on the registry name")
}

func TestUnit_LegacyChainPrefixHint(t *testing.T) {
	require.Contains(t, legacyChainPrefixHint("chain-reviewer"), `"reviewer"`)
	require.Empty(t, legacyChainPrefixHint("reviewer"), "a name without the prefix gets no hint")
	require.Empty(t, legacyChainPrefixHint("chain-"), "a bare prefix suggests nothing")
}

// A stale name from the previous docs must fail by explaining the rename, not
// by reading as a missing agent.
func TestUnit_AgentShow_StaleChainPrefixExplainsTheRename(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "agents.db")
	seedChainAgent(t, dbPath, "reviewer", "/tmp/chain-agent-reviewer.json")

	show := &cobra.Command{Use: "show", Args: cobra.ExactArgs(1), RunE: agentShowCmd.RunE}
	root := agentTestRoot(show)
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"--db", dbPath, "show", "chain-reviewer"})

	err := root.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "chain- prefix")
	require.Contains(t, err.Error(), `"reviewer"`)
}
