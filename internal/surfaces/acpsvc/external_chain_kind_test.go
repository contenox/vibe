package acpsvc

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/contenox/beam/internal/kernel/agentinstance"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/store/runtimetypes"
	libacp "github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// C9 made `chain` a first-class fleet unit and left one loose end: the CHAT path
// (`contenox.agent` in session/new) asked every declared agent for an
// external_acp config, which a chain agent does not have — so a discovered chain
// agent was visible in the picker and refused with a kind-mismatch the moment
// anyone selected it. These tests pin both halves of the repair: the
// Manager-owned path runs it, and the connection-owned path — which genuinely
// cannot — refuses it honestly instead of spawning something wrong.

// registerChainAgentInDB declares a chain-kind agent through the normal registry
// path. The chain file is never read here: the spawn is redirected at the kernel
// (WithSelfExecutable), and ChainConfig deliberately does not stat its path.
func registerChainAgentInDB(t *testing.T, db libdb.DBManager, name string) string {
	t.Helper()
	svc := agentregistryservice.New(db)
	agent := &runtimetypes.Agent{Name: name, Enabled: true}
	require.NoError(t, agent.SetChainConfig(runtimetypes.ChainConfig{
		Path:    filepath.Join(t.TempDir(), "agent-chat-fixture.json"),
		ChainID: "agent-chat-fixture",
	}))
	require.NoError(t, svc.Create(context.Background(), agent))
	return name
}

// A chain agent selected in chat opens a session and drives a prompt turn like
// any other unit. The Manager already knew how to spawn one (StartResolved's
// chain branch); the chat path just had to stop demanding an external_acp config
// it was never going to have.
func TestLoopback_ChainAgent_ChatPathOpensAndPrompts(t *testing.T) {
	stub := buildStubAgentBin(t)
	f := newInstancesFixtureWith(t, func(db libdb.DBManager) agentinstance.Manager {
		// The chain branch re-executes THIS binary bound to a chain file. Under
		// `go test` that is the test binary, which serves no ACP, so point the
		// self-spawn at the stub — the same seam the kernel's own chain tests
		// use. What is under test here is the chat path's RESOLUTION of the
		// kind, not the chain engine behind it.
		return agentinstance.New(agentregistryservice.New(db), agentinstance.WithSelfExecutable(stub))
	})
	agentName := registerChainAgentInDB(t, f.db, "chain-chat-fixture")

	conn := f.connect()
	ctx := context.Background()
	_, err := conn.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := conn.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err, "a chain agent offered in the picker must be usable in chat")
	require.Equal(t, agentName, metaAgent(t, newResp.Meta))

	promptResp, err := conn.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("ping")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, promptResp.StopReason)
}

// The connection-owned (stdio, nil-Instances) path spawns from the external_acp
// config in person, and a chain agent has none. It must REFUSE — naming the
// remedy — rather than building a subprocess out of the deliberately zero config
// resolveExternalAgent hands it.
func TestLoopback_ChainAgent_ConnCtxPathRefusesHonestly(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	require.Nil(t, h.tr.deps.Instances, "this harness is the nil-Instances path by construction")

	agentName := registerChainAgentInDB(t, h.tr.deps.DB, "chain-stdio-fixture")

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	_, err = h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "chain agent")
	require.Contains(t, err.Error(), "/mission",
		"the refusal must name where a chain unit CAN run, not just that this cannot run it")

	h.tr.sessionMu.Lock()
	n := len(h.tr.sessions)
	h.tr.sessionMu.Unlock()
	require.Zero(t, n, "a refused agent must not create a session")
}
