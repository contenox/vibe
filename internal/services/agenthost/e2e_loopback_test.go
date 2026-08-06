package agenthost_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/internal/libdbexec"
	"github.com/contenox/contenox/internal/services/agenthost"
	"github.com/contenox/contenox/internal/services/agentregistryservice"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	"github.com/contenox/contenox/libacp/acpexec"
	"github.com/stretchr/testify/require"
)

// This file is the self-hosting loopback e2e: contenox's own binary,
// registered pointing at `contenox acp`, driven through the same registry →
// DriveTurn path as any other agent. Determinism comes from a fixture chain,
// not a model.

// loopbackFixtureReply must match the `print` of writeLoopbackChain's noop task.
const loopbackFixtureReply = "contenox loopback fixture reply"

// buildContenoxBinary compiles cmd/contenox into t.TempDir() and returns its path.
func buildContenoxBinary(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "contenox")
	cmd := exec.Command("go", "build", "-o", binPath, "github.com/contenox/contenox/cmd/contenox")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build contenox: %v\n%s", err, out)
	}
	return binPath
}

// runContenoxCLI runs the built binary with HOME and Cwd pinned to home.
func runContenoxCLI(t *testing.T, bin, home string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("contenox %v: %v\n%s", args, err, out)
	}
}

// writeLoopbackChain writes the fixture chain (one noop task) and returns its path.
func writeLoopbackChain(t *testing.T, dir string) string {
	t.Helper()
	chain := taskengine.TaskChainDefinition{
		ID:          "loopback-e2e-fixture",
		Description: "Deterministic no-model fixture: one noop task whose print is the reply.",
		Tasks: []taskengine.TaskDefinition{{
			ID:          "reply",
			Description: "Answer every prompt with the fixed fixture text.",
			Handler:     taskengine.HandleNoop,
			Print:       loopbackFixtureReply,
		}},
	}
	data, err := json.Marshal(chain)
	require.NoError(t, err)
	path := filepath.Join(dir, "loopback-chain.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

// registerLoopbackAgent is registerAgent plus Env (isolated HOME, chain path).
func registerLoopbackAgent(t *testing.T, name, command string, args []string, env map[string]string) (context.Context, *runtimetypes.Agent) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "agenthost-e2e.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	svc := agentregistryservice.New(db)
	agent := &runtimetypes.Agent{Name: name, Enabled: true}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   command,
		Args:      args,
		Env:       env,
	}))
	require.NoError(t, svc.Create(ctx, agent))

	resolved, err := svc.GetByName(ctx, name)
	require.NoError(t, err)
	return ctx, resolved
}

// TestHostE2E_Loopback_DeterministicChainReply pins the fixture chain's
// byte-exact reply through the registry → DriveTurn → contenox acp path.
func TestHostE2E_Loopback_DeterministicChainReply(t *testing.T) {
	// The sandbox wall forces HOME to the real home and denies ~/.contenox.
	t.Skip("self-hosting loopback is incompatible with the sandbox-only spawn path: " +
		"the wall forces HOME to the operator's real home and denies ~/.contenox, so a " +
		"confined `contenox acp` cannot reach the isolated state this test seeds (see file comment)")

	if testing.Short() {
		t.Skip("skipping loopback e2e: builds and boots the full contenox binary")
	}

	bin := buildContenoxBinary(t)
	home := t.TempDir()

	// The fake model name makes accidental resolution fail loudly.
	runContenoxCLI(t, bin, home, "config", "set", "default-model", "loopback-fixture-model")
	runContenoxCLI(t, bin, home, "config", "set", "update-check", "false")

	chainPath := writeLoopbackChain(t, home)

	// The rest neutralize ambient CONTENOX_DEFAULT_* overrides (empty = unset).
	env := map[string]string{
		"HOME":                          home,
		"CONTENOX_ACP_CHAIN_PATH":       chainPath,
		"CONTENOX_DEFAULT_MODEL":        "",
		"CONTENOX_DEFAULT_PROVIDER":     "",
		"CONTENOX_DEFAULT_ALT_MODEL":    "",
		"CONTENOX_DEFAULT_ALT_PROVIDER": "",
		"CONTENOX_DEFAULT_MAX_TOKENS":   "",
		"CONTENOX_DEFAULT_THINK":        "",
	}

	ctx, agent := registerLoopbackAgent(t, "contenox-loopback", bin, []string{"acp", "--auto"}, env)

	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	var stderr acpexec.LockedBuffer
	harness := &agenthost.RecordingHarness{}
	res, err := agenthost.DriveTurn(ctx, agent, harness, agenthost.TurnRequest{
		Cwd:       t.TempDir(),
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("ping through the loopback")},
		Stderr:    &stderr,
		KillGrace: 2 * time.Second,
	})
	require.NoError(t, err, "contenox acp stderr:\n%s", stderr.String())

	require.Equal(t, libacp.ProtocolVersion, res.Initialize.ProtocolVersion)
	require.NotNil(t, res.Initialize.AgentInfo)
	require.Equal(t, "contenox", res.Initialize.AgentInfo.Name)
	require.NotEmpty(t, res.SessionID)

	require.Equal(t, libacp.StopReasonEndTurn, res.StopReason,
		"contenox acp stderr:\n%s", stderr.String())

	require.Equal(t, loopbackFixtureReply, harness.MessageText(),
		"streamed reply must be exactly the fixture chain's print output; all updates: %#v", harness.Updates())

	tracker := &libacp.TurnTracker{}
	for _, n := range harness.Updates() {
		tracker.Observe(n)
	}
	require.NoError(t, tracker.Err(res.StopReason))
}
