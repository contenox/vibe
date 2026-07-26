package contenoxcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnit_LoadAgentsMD_FoundInStartDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	require.NoError(t, os.WriteFile(path, []byte("# Project rules\nUse local_fs."), 0644))

	content, found, ok := LoadAgentsMD(dir)
	require.True(t, ok)
	require.Equal(t, path, found)
	require.Contains(t, content, "Use local_fs.")
}

func TestUnit_LoadAgentsMD_FoundInParent(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root rules"), 0644))
	leaf := filepath.Join(root, "a", "b", "c")
	require.NoError(t, os.MkdirAll(leaf, 0755))

	content, found, ok := LoadAgentsMD(leaf)
	require.True(t, ok)
	require.Equal(t, filepath.Join(root, "AGENTS.md"), found)
	require.Equal(t, "root rules", content)
}

func TestUnit_LoadAgentsMD_ClosestWins(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root"), 0644))
	subdir := filepath.Join(root, "sub")
	require.NoError(t, os.MkdirAll(subdir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(subdir, "AGENTS.md"), []byte("sub"), 0644))

	content, found, ok := LoadAgentsMD(subdir)
	require.True(t, ok)
	require.Equal(t, filepath.Join(subdir, "AGENTS.md"), found, "closest AGENTS.md must win, not the root one")
	require.Equal(t, "sub", content)
}

func TestUnit_LoadAgentsMD_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, _, ok := LoadAgentsMD(dir)
	require.False(t, ok, "no AGENTS.md anywhere up the tree must return ok=false")
}

func TestUnit_LoadAgentsMD_TruncatedWhenOversized(t *testing.T) {
	dir := t.TempDir()
	bigContent := strings.Repeat("x", 80*1024)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(bigContent), 0644))

	content, _, ok := LoadAgentsMD(dir)
	require.True(t, ok)
	require.LessOrEqual(t, len(content), maxAgentsMDBytes+len(agentsMDTruncated))
	require.Contains(t, content, "AGENTS.md truncated")
}

func TestUnit_AgentsMDMessage_IncludesPathAndContent(t *testing.T) {
	msg := AgentsMDMessage("Use TypeScript strict mode.", "/some/path/AGENTS.md")
	require.Equal(t, "system", msg.Role)
	require.Contains(t, msg.Content, "/some/path/AGENTS.md")
	require.Contains(t, msg.Content, "Use TypeScript strict mode.")
	require.Contains(t, msg.Content, "AGENTS.md", "should reference the standard by name so the model knows what it is")
}
