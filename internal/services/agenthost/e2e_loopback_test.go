package agenthost_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/taskengine"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/agenthost"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
	"github.com/contenox/beam/libacp/acpexec"
	"github.com/stretchr/testify/require"
)

// This file is the self-hosting loopback e2e from
// docs/development/blueprints/acp/agent-servers-and-client-e2e.md: contenox's
// own binary, freshly built, is registered as an external ACP agent pointing
// at `contenox acp` and driven by this repo's client-host. One DriveTurn walks
// registry row → resolve → spawn → initialize → session/new → session/prompt →
// task-chain execution inside the spawned contenox → streamed reply →
// teardown, with no piece mocked and no LLM backend, GPU, or network anywhere.
//
// Determinism comes from the chain, not a model: `contenox acp` executes
// whatever chain CONTENOX_ACP_CHAIN_PATH points at (runtime/acpsvc/chain.go),
// and a single `noop`-handler task never touches a model while its `print`
// template is published as a TaskEventPrint, which acpsvc translates into an
// agent_message_chunk (runtime/acpsvc/events.go) — the exact reply text the
// harness records.
//
// State isolation: every state path `contenox acp` reads — the SQLite DB, the
// seeded chain/HITL presets, workspace id — resolves under $HOME/.contenox
// via os.UserHomeDir() (runtime/contenoxcli/backend_cmd.go globalContenoxDir).
// The spawned agent (and the `contenox config set` seeding runs) therefore get
// HOME pointed at a per-test temp dir, so the user's real ~/.contenox is never
// touched. os/exec deduplicates Cmd.Env keeping the last entry, so appending
// HOME to os.Environ() reliably overrides it for the child.

// loopbackFixtureReply is the byte-exact reply the fixture chain streams. It
// must match the `print` of the single noop task in writeLoopbackChain.
const loopbackFixtureReply = "contenox loopback fixture reply"

// buildContenoxBinary compiles cmd/contenox into t.TempDir() and returns its
// path, mirroring buildStubAgent (externalacp_test.go); the go build cache
// makes reruns cheap.
func buildContenoxBinary(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "contenox")
	cmd := exec.Command("go", "build", "-o", binPath, "github.com/contenox/beam/cmd/contenox")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build contenox: %v\n%s", err, out)
	}
	return binPath
}

// runContenoxCLI runs the built binary against the isolated HOME — the same
// real CLI surface a user seeds state with (`contenox config set …`). Cwd is
// pinned to home so no cwd-walking code can escape into the repo's .contenox.
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

// writeLoopbackChain writes the deterministic no-model fixture chain and
// returns its path. One leaf noop task (no branches ends the chain cleanly,
// see taskengine.SimpleEnv.evaluateTransitions): the noop handler never
// resolves a model, and its print template is the entire streamed reply.
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

// registerLoopbackAgent walks the same real registration leg as registerAgent
// (drive_test.go) — registry service on a fresh SQLite DB, create, resolve by
// name — but builds the row inline because the loopback config additionally
// needs Env (isolated HOME, fixture chain path), which the shared helper's
// command+args signature does not carry.
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

// TestHostE2E_Loopback_DeterministicChainReply is the full self-hosting
// loopback: a registry row pointing at the freshly built contenox binary's
// `acp` subcommand, resolved and driven through DriveTurn for one prompt
// turn, asserting the exact reply text the fixture chain streams and a clean
// end_turn stop. `--auto` disables HITL prompts (the RecordingHarness cannot
// answer permission requests; the noop chain never asks anyway).
func TestHostE2E_Loopback_DeterministicChainReply(t *testing.T) {
	// KNOWN INCOMPATIBILITY with the sandbox-only spawn path. This self-hosting
	// loopback boots `contenox acp` and relies on its state (SQLite DB, seeded
	// presets) resolving under a per-test isolated $HOME/.contenox. But the wall
	// (1) FORCES $HOME to the operator's real home (libsandbox scrubEnv overrides
	// any inherited/EnvSet HOME with Spec.Home = os.UserHomeDir()), so the confined
	// process no longer sees the test's temp HOME, and (2) DENIES ~/.contenox by
	// construction (it is not carved — the control-plane isolation invariant). So
	// the confined `contenox acp` cannot reach the state this test seeds. It is
	// skipped, not weakened: re-enabling it needs a per-agent scoped home plus a
	// control-plane carve-out mechanism the fixed Spec does not (yet) provide.
	t.Skip("self-hosting loopback is incompatible with the sandbox-only spawn path: " +
		"the wall forces HOME to the operator's real home and denies ~/.contenox, so a " +
		"confined `contenox acp` cannot reach the isolated state this test seeds (see file comment)")

	if testing.Short() {
		t.Skip("skipping loopback e2e: builds and boots the full contenox binary")
	}

	bin := buildContenoxBinary(t)
	home := t.TempDir()

	// Seed the isolated state through the real CLI. enginesvc.Build (and with
	// it session/new) hard-requires a configured default-model even though the
	// noop fixture chain never resolves one; the name is deliberately fake so
	// any accidental model resolution fails loudly instead of finding a real
	// backend. update-check=false keeps startup off the network entirely
	// (acpUpdateBanner short-circuits before its goroutine spawns).
	runContenoxCLI(t, bin, home, "config", "set", "default-model", "loopback-fixture-model")
	runContenoxCLI(t, bin, home, "config", "set", "update-check", "false")

	chainPath := writeLoopbackChain(t, home)

	env := map[string]string{
		// Full state isolation: every path `contenox acp` reads (DB, seeded
		// presets, workspace id, telemetry) resolves under $HOME/.contenox.
		"HOME": home,
		// The chain that answers the session: CONTENOX_ACP_CHAIN_PATH
		// overrides ~/.contenox/default-acp-chain.json (acpsvc.LoadChainRegistryFrom).
		"CONTENOX_ACP_CHAIN_PATH": chainPath,
		// Neutralize ambient CONTENOX_DEFAULT_* overrides from the invoking
		// environment: they are read env-first on every acp launch
		// (configValueWithEnv) and a leaked value could redirect the boot or
		// fail it outright (default-think/default-max-tokens are validated).
		// Empty values read as unset, falling through to the seeded DB config.
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

	// The spawned agent really is contenox serving ACP, on the shared
	// protocol version.
	require.Equal(t, libacp.ProtocolVersion, res.Initialize.ProtocolVersion)
	require.NotNil(t, res.Initialize.AgentInfo)
	require.Equal(t, "contenox", res.Initialize.AgentInfo.Name)
	require.NotEmpty(t, res.SessionID)

	// The chain ran to its leaf and the turn closed normally.
	require.Equal(t, libacp.StopReasonEndTurn, res.StopReason,
		"contenox acp stderr:\n%s", stderr.String())

	// The reply is the fixture chain's print — byte-exact, no model involved.
	// MessageText concatenates every agent_message_chunk of the turn, so this
	// also proves nothing else (banner, stray step output) leaked into the
	// reply stream.
	require.Equal(t, loopbackFixtureReply, harness.MessageText(),
		"streamed reply must be exactly the fixture chain's print output; all updates: %#v", harness.Updates())

	// Turn-level conformance: the turn produced displayable output for its
	// stop reason (same check the stub round-trip pins).
	tracker := &libacp.TurnTracker{}
	for _, n := range harness.Updates() {
		tracker.Observe(n)
	}
	require.NoError(t, tracker.Err(res.StopReason))
}
