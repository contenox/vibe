package agenthost

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/stretchr/testify/require"
)

// TestUnit_BuildAgentCmd_SelfInvocationIsNotConfined pins the one exemption from
// "every spawned agent runs inside the wall": a command that resolves to THIS
// running executable is contenox spawning contenox — a dispatched mission unit or
// a chain unit — and runs unconfined, because the wall would deny it the shared
// runtime state (~/.contenox is not carved) that being contenox is defined by.
// The observable difference is the assembled command: the target itself, with the
// parent's environment, rather than the /proc/self/exe shim re-exec.
func TestUnit_BuildAgentCmd_SelfInvocationIsNotConfined(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("CONTENOX_SELFSPAWN_PROBE", "inherited")

	cmd, err := buildAgentCmd(context.Background(), &ExternalACPAgent{
		Config: runtimetypes.ExternalACPConfig{
			Transport: runtimetypes.ExternalACPTransportStdio,
			Command:   self,
			Args:      []string{"acp"},
			Cwd:       t.TempDir(),
			Env:       map[string]string{"CONTENOX_ACP_CHAIN_PATH": "/somewhere/agent.json"},
		},
	})
	require.NoError(t, err, "a self-invocation must not be refused by the sandbox preflight")
	require.Equal(t, self, cmd.Path, "a self-invoked unit runs directly, not through the sandbox shim")
	require.Contains(t, cmd.Env, "CONTENOX_SELFSPAWN_PROBE=inherited",
		"a self-invoked unit inherits this process's environment — the shared state it is defined by")
	require.Contains(t, cmd.Env, "CONTENOX_ACP_CHAIN_PATH=/somewhere/agent.json",
		"the declared env is layered on top of the inherited one")
}

// TestUnit_BuildAgentCmd_ForeignCommandIsConfined is the other half: the very same
// binary contents at a DIFFERENT path is a foreign agent, so it goes through the
// wall. Identity is decided by file identity (os.SameFile), which is what keeps
// the exemption from being reachable by copying or renaming a program.
//
// Where the host cannot build the wall at all, buildAgentCmd fails closed — that
// is the contract, and it is asserted as such rather than skipped.
func TestUnit_BuildAgentCmd_ForeignCommandIsConfined(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	content, err := os.ReadFile(self)
	require.NoError(t, err)
	foreign := filepath.Join(t.TempDir(), "not-contenox")
	require.NoError(t, os.WriteFile(foreign, content, 0o755))

	cmd, err := buildAgentCmd(context.Background(), &ExternalACPAgent{
		Config: runtimetypes.ExternalACPConfig{
			Transport: runtimetypes.ExternalACPTransportStdio,
			Command:   foreign,
			Cwd:       t.TempDir(),
		},
	})
	if err != nil {
		require.Contains(t, err.Error(), "sandbox",
			"a host that cannot confine must refuse to run the agent, naming the sandbox")
		return
	}
	require.NotEqual(t, foreign, cmd.Path,
		"a foreign agent is reached through the sandbox shim, never exec'd directly")
}
