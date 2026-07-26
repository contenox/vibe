package fleetservice

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/libtracker"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/chainagents"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// This file is the acceptance for chains-as-agents: one of the runtime's own
// task chains is declared by convention, dispatched as a fleet unit, and
// answers. It closes the loop the rest of this slice's unit tests each cover a
// segment of — a passing unit test that never spawns proves nothing about a
// kind whose entire implementation is "re-execute this binary bound to a chain".
//
// Nothing here is mocked and no LLM, GPU, or network is involved:
//
//   - the binary is this repo's, freshly built;
//   - the chain agent is DISCOVERED from a file named by the agent-* convention,
//     not hand-registered, so the discovery half and the runnable half are proven
//     together (they are required to land together — a selectable agent that
//     fails at spawn is the exact half-built surface this must not manufacture);
//   - dispatch goes through the real fleetservice → agentinstance kernel →
//     agenthost spawn path, with the real registry and mission store under it;
//   - determinism comes from the chain, not a model: a single noop-handler task
//     never resolves a model, and its `print` template is published as a task
//     event that the spawned runtime's ACP layer translates into an
//     agent_message_chunk — the exact reply text asserted below.
//
// HOME is isolated to a per-test temp dir so the developer's real state is never
// touched. That isolation is also what makes the assertion meaningful in the
// other direction: the spawned unit reads its default-model configuration out of
// $HOME/.contenox, and the kernel sets ONLY the chain-path variable on the child,
// so a reply coming back at all is proof the child inherited this process's
// environment — which is the point of a chain unit (it shares the one global
// state) and the deliberate difference from an external agent that brings its
// own everything.

// chainFixtureReply is the byte-exact reply the fixture chain streams. It must
// match the `print` of the single noop task in writeChainAgentFixture.
const chainFixtureReply = "contenox chain unit fixture reply"

// buildContenoxBinary compiles cmd/contenox into t.TempDir() and returns its
// path; the go build cache makes reruns cheap.
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

// runContenoxCLI seeds state through the real CLI — the same surface a user
// configures with. Cwd is pinned to home so no cwd-walking code can escape into
// the repo's own .contenox.
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

// writeChainAgentFixture writes the deterministic no-model chain under a name
// that DECLARES it as an agent (the agent-* filename convention), and returns
// its path. One leaf noop task: the handler never resolves a model, and its
// print template is the entire streamed reply.
func writeChainAgentFixture(t *testing.T, contenoxDir string) string {
	t.Helper()
	chain := map[string]any{
		"id":          "agent-fleet-fixture",
		"description": "Deterministic no-model chain unit: one noop task whose print is the reply.",
		"tasks": []map[string]any{{
			"id":          "reply",
			"description": "Answer every prompt with the fixed fixture text.",
			"handler":     "noop",
			"print":       chainFixtureReply,
		}},
	}
	data, err := json.Marshal(chain)
	require.NoError(t, err)
	path := filepath.Join(contenoxDir, "agent-fleet-fixture.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

// recordingViewer is a Viewer that records the session stream. Deliver must not
// block (kernel contract), so it only appends under a mutex.
type recordingViewer struct {
	id string

	mu      sync.Mutex
	updates []libacp.SessionNotification
}

func (v *recordingViewer) ID() string { return v.id }

func (v *recordingViewer) Deliver(_ context.Context, n libacp.SessionNotification) error {
	v.mu.Lock()
	v.updates = append(v.updates, n)
	v.mu.Unlock()
	return nil
}

func (v *recordingViewer) RequestPermission(_ context.Context, _ libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
	// The fixture chain calls no tools, so nothing is ever gated here. Denying
	// is the safe answer if that ever changes.
	return libacp.RequestPermissionResponse{
		Outcome: libacp.RequestPermissionOutcome{Outcome: libacp.PermissionOutcomeCancelled},
	}, nil
}

// messageText concatenates the text of every agent_message_chunk observed: the
// unit's streamed reply as one string.
func (v *recordingViewer) messageText() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var sb strings.Builder
	for _, n := range v.updates {
		if n.Update.SessionUpdate != libacp.SessionUpdateAgentMessageChunk {
			continue
		}
		if c := n.Update.Content; c != nil && c.Type == string(libacp.ContentKindText) {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// TestFleetE2E_ChainAgent_DiscoveredDispatchedAndAnswers walks the whole slice:
// a chain file named by convention → a discovered registry row of kind "chain"
// → fleetservice.Dispatch → the kernel re-executing this binary's ACP server
// bound to that chain → the chain running → the reply streaming back to an
// attached viewer.
func TestFleetE2E_ChainAgent_DiscoveredDispatchedAndAnswers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping chain-unit e2e: builds and boots the full contenox binary")
	}

	bin := buildContenoxBinary(t)
	home := t.TempDir()

	// Full state isolation for the spawned unit: every path it reads (its DB,
	// the seeded presets, the workspace id) resolves under $HOME/.contenox. The
	// kernel sets no HOME on the child — it inherits THIS process's — so setting
	// it here is what redirects the child, and is also what proves inheritance
	// when the unit answers.
	t.Setenv("HOME", home)
	// Neutralize ambient overrides from the invoking environment: they are read
	// env-first on every launch, and a leaked value could redirect the boot or
	// fail it outright. Empty reads as unset, falling through to the seeded DB
	// configuration.
	for _, k := range []string{
		"CONTENOX_DEFAULT_MODEL", "CONTENOX_DEFAULT_PROVIDER",
		"CONTENOX_DEFAULT_ALT_MODEL", "CONTENOX_DEFAULT_ALT_PROVIDER",
		"CONTENOX_DEFAULT_MAX_TOKENS", "CONTENOX_DEFAULT_THINK",
		"CONTENOX_ACP_CHAIN_PATH",
	} {
		t.Setenv(k, "")
	}

	// Seed the isolated state through the real CLI. The engine hard-requires a
	// configured default model even though the noop fixture chain never resolves
	// one; the name is deliberately fake so any accidental model resolution
	// fails loudly instead of finding a real backend. update-check=false keeps
	// startup off the network entirely.
	runContenoxCLI(t, bin, home, "config", "set", "default-model", "chain-unit-fixture-model")
	runContenoxCLI(t, bin, home, "config", "set", "update-check", "false")

	contenoxDir := filepath.Join(home, ".contenox")
	require.DirExists(t, contenoxDir, "the CLI seeding run must have created the isolated state directory")
	chainPath := writeChainAgentFixture(t, contenoxDir)

	// The fleet's own stores. A registry DB of its own rather than the unit's:
	// the unit has no business reading the agents table, and sharing one sqlite
	// file across a subprocess boundary would test locking, not dispatch.
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "fleet-e2e.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	agents := agentregistryservice.New(db)
	missions := missionservice.New(db)

	// DECLARE BY CONVENTION: no hand-registration anywhere in this test.
	res, err := chainagents.Discover(ctx, agents, contenoxDir)
	require.NoError(t, err)
	require.Equal(t, []string{"agent-fleet-fixture"}, res.Created,
		"naming the file agent-*.json is the whole declaration")

	declared, err := agents.GetByName(ctx, "agent-fleet-fixture")
	require.NoError(t, err)
	require.Equal(t, runtimetypes.AgentKindChain, declared.Kind)
	cfg, err := declared.ChainConfig()
	require.NoError(t, err)
	require.Equal(t, chainPath, cfg.Path)

	// The real kernel, pointed at the freshly built binary (under `go test`
	// os.Executable() is the test binary, which serves no ACP).
	stderr := &lockedBuffer{}
	instances := agentinstance.New(agents,
		agentinstance.WithSelfExecutable(bin),
		agentinstance.WithStderr(stderr),
	)
	t.Cleanup(func() { _ = instances.Close() })

	workDir := t.TempDir()
	svc := New(instances, agents, missions, nil, workDir, libtracker.NoopTracker{})

	result, err := svc.Dispatch(ctx, DispatchRequest{
		AgentName:      "agent-fleet-fixture",
		Intent:         "answer through the chain unit",
		HITLPolicyName: "default",
	})
	require.NoError(t, err, "chain unit stderr:\n%s", stderr.String())
	require.NotEmpty(t, result.InstanceID)
	require.NotEmpty(t, result.SessionID)
	require.NotEmpty(t, result.MissionID, "every dispatch is a mission, chain units included")

	// Attach as a viewer to observe the unit's stream. Dispatch runs the intent
	// as a detached first turn, so the attach may land mid-turn; the journal
	// replay covers whatever already streamed.
	viewer := &recordingViewer{id: "e2e-observer"}
	controller, err := instances.Attach(ctx, result.InstanceID, libacp.SessionID(result.SessionID), viewer)
	require.NoError(t, err)
	require.True(t, controller, "the first viewer of an unattended dispatched session becomes its controller")

	// The reply is the fixture chain's print — no model involved. Contains, not
	// byte-equality: this fixture calls no mission tool, so under the mission
	// doctrine its turn ends bare and the runtime nudges it once (see
	// driveUnattendedMission), and the deterministic unit answers the nudge by
	// printing the same reply a second time. The cure's own behavior — heartbeat,
	// the one nudge, the runtime-filed blocker — is asserted in
	// e2e_unattended_nudge_test.go; here we only need the reply to have streamed.
	require.Eventually(t, func() bool {
		return strings.Contains(viewer.messageText(), chainFixtureReply)
	}, 120*time.Second, 100*time.Millisecond,
		"chain unit never streamed its reply; got %q\nstderr:\n%s", viewer.messageText(), stderr.String())

	// The board sees it as a running unit of kind chain, with the session open.
	status, err := svc.Get(ctx, result.InstanceID)
	require.NoError(t, err)
	require.Equal(t, agentinstance.StateRunning, status.State)
	require.Equal(t, runtimetypes.AgentKindChain, status.Kind)
	require.Contains(t, status.SessionIDs, result.SessionID)

	// And it stops like any other unit.
	require.NoError(t, svc.Stop(ctx, result.InstanceID))
	_, err = svc.Get(ctx, result.InstanceID)
	require.ErrorIs(t, err, agentinstance.ErrNotFound)
}

// lockedBuffer is a concurrency-safe sink for the spawned unit's stderr, so a
// failure message can quote it without racing the subprocess's writer.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
