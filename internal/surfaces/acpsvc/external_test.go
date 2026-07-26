package acpsvc

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/contenox/beam/internal/kernel/enginesvc"
	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/services/agentregistryservice"
	"github.com/contenox/beam/internal/services/clikv"
	"github.com/contenox/beam/internal/store/runtimetypes"
	libacp "github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// This file exercises the external-agent-backed ACP session: a session whose
// contenox.agent `_meta` binds it to a REGISTERED external ACP agent, spawned
// and driven downstream via runtime/agenthost, instead of the native chain
// engine. It runs the REAL production Transport against a REAL
// ClientSideConnection (via newLoopbackHarness), and the downstream side is the
// hermetic in-repo acp-stub-agent — no LLM backend, no mocked host seam.

// buildStubAgentBin compiles libacp/cmd/acp-stub-agent into t.TempDir() and
// returns its path, mirroring agenthost's buildStubAgent. The go build cache
// makes reruns cheap.
func buildStubAgentBin(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "acp-stub-agent")
	cmd := exec.Command("go", "build", "-o", binPath, "github.com/contenox/beam/libacp/cmd/acp-stub-agent")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build acp-stub-agent: %v\n%s", err, out)
	}
	return binPath
}

// registerStubAgentInDB creates an enabled external_acp agents row pointing at a
// freshly built stub agent in the given DB — the same DB the Transport resolves
// the contenox.agent name against — carrying an optional per-agent env (used to
// opt the stub into ACP_STUB_ADVERTISE_COMMANDS without a process-global setenv),
// and returns the registered name.
func registerStubAgentInDB(t *testing.T, db libdb.DBManager, name string, env map[string]string) string {
	t.Helper()
	bin := buildStubAgentBin(t)
	svc := agentregistryservice.New(db)
	agent := &runtimetypes.Agent{Name: name, Enabled: true}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   bin,
		Env:       env,
	}))
	require.NoError(t, svc.Create(context.Background(), agent))
	return name
}

// registerStubAgent registers a default (non-advertising) stub in the harness's
// own DB.
func registerStubAgent(t *testing.T, h *loopbackHarness, name string) string {
	t.Helper()
	return registerStubAgentInDB(t, h.tr.deps.DB, name, nil)
}

// metaAgent reads the contenox.agent value out of a `_meta` blob, failing if it
// is absent.
func metaAgent(t *testing.T, meta json.RawMessage) string {
	t.Helper()
	require.NotEmpty(t, meta, "expected _meta with contenox.agent")
	return parseAgentMeta(meta)
}

// TestLoopback_ExternalAgent_NewSessionAndPromptRelays is the keystone: a
// session/new carrying contenox.agent against a registered stub agent spawns and
// drives it downstream, the response `_meta` echoes the key, external sessions
// advertise no chain-engine model/think/token selects (only contenox's own
// per-session HITL policy select, since a non-advertising stub carries no
// downstream surface), and a prompt turn relays the stub's "ack" chunk up on the
// UPSTREAM session id and returns end_turn.
func TestLoopback_ExternalAgent_NewSessionAndPromptRelays(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	agentName := registerStubAgent(t, h, "claude-stub")

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)
	require.NotEmpty(t, newResp.SessionID)
	require.Equal(t, agentName, metaAgent(t, newResp.Meta),
		"session/new response _meta must echo contenox.agent")
	require.Len(t, newResp.ConfigOptions, 1,
		"a non-advertising external agent surfaces no chain-engine selects — only contenox's own HITL policy select")
	require.Equal(t, configIDHITLPolicy, newResp.ConfigOptions[0].ID,
		"contenox's HITL policy select is the sole config option of a modes-and-config-less external session")

	promptResp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("hello from beam")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, promptResp.StopReason)

	// The stub's plain-prompt path acks with one agent_message_chunk; the bridge
	// relays it up, plus the post-turn session_info_update.
	updates := h.lc.drain(t, 2)
	byKind := make(map[libacp.SessionUpdateKind]libacp.SessionNotification, len(updates))
	for _, u := range updates {
		byKind[u.Update.SessionUpdate] = u
	}
	chunk, ok := byKind[libacp.SessionUpdateAgentMessageChunk]
	require.True(t, ok, "the downstream agent's message chunk must be relayed upstream")
	require.Equal(t, newResp.SessionID, chunk.SessionID,
		"a relayed downstream update must be remapped onto the upstream session id")
	require.NotNil(t, chunk.Update.Content)
	require.Equal(t, "ack", chunk.Update.Content.Text)
	require.Contains(t, byKind, libacp.SessionUpdateSessionInfo,
		"an external turn still pushes the post-turn session_info_update")
}

// TestLoopback_ExternalAgent_AcceptsMentionResourceLink proves an @-mention works
// on an external session. beam's composer serializes a mention as a reference-only
// resource_link content block (promptBlocksFromDraft), and externalDriver.Prompt
// forwards the prompt blocks VERBATIM downstream. resource_link is a base ACP
// content type gated by NO promptCapability (embeddedContext gates only embedded
// `resource` blocks, which this path never emits), so it needs no downstream
// capability and requires no degrade. The turn completes (the stub acks) and the
// mention reference is persisted into the session's history, so session/list
// reflects it — proof the block reached the external driver's Prompt path intact.
func TestLoopback_ExternalAgent_AcceptsMentionResourceLink(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	agentName := registerStubAgent(t, h, "claude-stub-mention")

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)

	// A text block plus one @-mention as a resource_link — exactly what beam's
	// promptBlocksFromDraft puts on the wire for `review @main.go`.
	promptResp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt: []libacp.ContentBlock{
			libacp.NewTextContent("review"),
			{Type: string(libacp.ContentKindResourceLink), Name: "main.go", URI: "main.go"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, promptResp.StopReason,
		"an external turn carrying an @-mention resource_link must complete normally")

	// The downstream stub acks; the bridge relays it plus the post-turn info update.
	updates := h.lc.drain(t, 2)
	var acked bool
	for _, u := range updates {
		if u.Update.SessionUpdate == libacp.SessionUpdateAgentMessageChunk && u.Update.Content != nil {
			require.Equal(t, "ack", u.Update.Content.Text)
			acked = true
		}
	}
	require.True(t, acked, "the downstream agent must complete the turn (no capability rejection)")

	// The mention reference survives into persisted history, so session/list shows
	// it — proof the resource_link block reached the external driver's Prompt path.
	listResp, err := h.client.ListSessions(ctx, libacp.ListSessionsRequest{})
	require.NoError(t, err)
	var found *libacp.SessionInfo
	for i := range listResp.Sessions {
		if listResp.Sessions[i].SessionID == newResp.SessionID {
			found = &listResp.Sessions[i]
			break
		}
	}
	require.NotNil(t, found, "the external session must appear in session/list")
	require.Contains(t, found.Title, "main.go",
		"the @-mention's resource_link reference must be persisted in the external session's history")
}

// TestLoopback_ExternalAgent_UnknownAgentRejected pins that an unknown
// contenox.agent name fails session/new with a clear error and creates NO
// session (and, resolving before any spawn, leaks no process).
func TestLoopback_ExternalAgent_UnknownAgentRejected(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	_, err = h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON("no-such-agent"),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown")

	h.tr.sessionMu.Lock()
	n := len(h.tr.sessions)
	h.tr.sessionMu.Unlock()
	require.Zero(t, n, "a rejected external agent must not create a session")
}

// TestLoopback_ExternalAgent_DisabledAgentRejected proves the connCtx-owned
// (nil-Instances / stdio `contenox acp`) spawn path refuses a disabled agent
// through the shared agentregistryservice.ResolveForSpawn judgment used by
// resolveExternalAgent — the actual C5 gap fleet-consolidation.md's D6 named:
// before this change only fleetservice.Dispatch enforced Enabled, and a
// disabled agent's session/new straight against acpsvc would have spawned its
// subprocess anyway. Uses /bin/true as the command: resolution is refused
// before anything is ever spawned, so no real stub binary is needed.
func TestLoopback_ExternalAgent_DisabledAgentRejected(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	const agentName = "claude-stub-disabled"
	svc := agentregistryservice.New(h.tr.deps.DB)
	agent := &runtimetypes.Agent{Name: agentName, Enabled: false}
	require.NoError(t, agent.SetExternalACPConfig(runtimetypes.ExternalACPConfig{
		Transport: runtimetypes.ExternalACPTransportStdio,
		Command:   "/bin/true",
	}))
	require.NoError(t, svc.Create(ctx, agent))

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	_, err = h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "disabled")
	require.Contains(t, err.Error(), "contenox agent enable",
		"the ACP-level error must name the remedy, matching fleetservice's dispatch-path wording")

	h.tr.sessionMu.Lock()
	n := len(h.tr.sessions)
	h.tr.sessionMu.Unlock()
	require.Zero(t, n, "a refused agent must not create a session")
}

// TestLoopback_ExternalAgent_NoMetaKeyIsNative is the regression guard at this
// layer: a session/new without the contenox.agent key takes the native chain
// path unchanged — it holds no external handle and advertises the usual config
// options.
func TestLoopback_ExternalAgent_NoMetaKeyIsNative(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)
	require.Empty(t, newResp.Meta, "a native session/new carries no contenox.agent _meta")
	require.NotEmpty(t, newResp.ConfigOptions, "a native session still advertises chain config options")

	h.tr.sessionMu.Lock()
	entry := h.tr.sessions[newResp.SessionID]
	h.tr.sessionMu.Unlock()
	require.NotNil(t, entry)
	require.IsType(t, &nativeDriver{}, entry.driver, "a native session must be backed by a native driver")
}

// TestLoopback_ExternalAgent_CloseTearsDownProcess proves an explicit
// session/close tears down the spawned downstream connection (its subprocess),
// so external agents do not leak past the session that owns them.
func TestLoopback_ExternalAgent_CloseTearsDownProcess(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	agentName := registerStubAgent(t, h, "claude-stub-close")

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)

	h.tr.sessionMu.Lock()
	entry := h.tr.sessions[newResp.SessionID]
	h.tr.sessionMu.Unlock()
	require.NotNil(t, entry)
	ext, ok := entry.driver.(*externalDriver)
	require.True(t, ok, "an external session must be backed by an external driver")
	ext.mu.Lock()
	handle := ext.handle
	ext.mu.Unlock()
	require.NotNil(t, handle, "an external session must hold a live downstream handle")

	_, err = h.client.CloseSession(ctx, libacp.CloseSessionRequest{SessionID: newResp.SessionID})
	require.NoError(t, err)

	select {
	case <-handle.Conn.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("downstream connection (spawned process) was not torn down on session/close")
	}
}

// TestLoopback_ExternalAgent_PersistsHistoryForListing proves an external turn's
// user prompt and downstream reply are persisted, so the session appears in
// session/list with a title derived from the first user message and carries its
// contenox.agent attribution in `_meta`.
func TestLoopback_ExternalAgent_PersistsHistoryForListing(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	agentName := registerStubAgent(t, h, "claude-stub-hist")

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)

	const firstPrompt = "summarize the repo layout"
	_, err = h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent(firstPrompt)},
	})
	require.NoError(t, err)

	listResp, err := h.client.ListSessions(ctx, libacp.ListSessionsRequest{})
	require.NoError(t, err)

	var found *libacp.SessionInfo
	for i := range listResp.Sessions {
		if listResp.Sessions[i].SessionID == newResp.SessionID {
			found = &listResp.Sessions[i]
			break
		}
	}
	require.NotNil(t, found, "the external session must appear in session/list")
	require.Equal(t, firstPrompt, found.Title,
		"session/list title must derive from the first user prompt")
	require.Equal(t, agentName, metaAgent(t, found.Meta),
		"session/list entry must carry contenox.agent attribution in _meta")
}

// TestE2E_Wire_ExternalAgent_CommandMenuAfterNewSessionResult is the regression
// for the dropped downstream slash-command menu. Driven at the raw wire against
// the real Transport (so notification-vs-response ORDER is observable), it pins
// that a downstream agent's available_commands_update — advertised immediately
// after its own session/new — is relayed to the upstream client STRICTLY AFTER
// the external session/new result, never before it (a client drops updates for a
// session id it has not yet learned, which is why the menu never rendered in
// beam). The downstream side is the hermetic acp-stub-agent opted into
// ACP_STUB_ADVERTISE_COMMANDS via its per-agent env.
func TestE2E_Wire_ExternalAgent_CommandMenuAfterNewSessionResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "wire-external.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer db.Close()

	agentName := registerStubAgentInDB(t, db, "claude-stub-wire",
		map[string]string{"ACP_STUB_ADVERTISE_COMMANDS": "1"})

	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentSide := &wirePipe{r: agentR, w: agentW}
	clientSide := &wirePipe{r: clientR, w: clientW}

	factory := New(Deps{
		// A bare engine is enough for the external session lifecycle here; no
		// native chain runs (the downstream stub owns the turn).
		Engine:      &enginesvc.Engine{},
		DB:          db,
		WorkspaceID: "wire-external-ws",
	})
	conn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		return factory(c)
	})
	runDone := make(chan error, 1)
	go func() { runDone <- conn.Run(ctx) }()
	defer func() {
		_ = clientSide.Close()
		select {
		case <-runDone:
		case <-time.After(2 * time.Second):
			t.Error("connection did not shut down")
		}
	}()

	client := &wireClient{t: t, rw: clientSide}

	resp, _ := client.call(libacp.MethodInitialize, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientInfo:      &libacp.Implementation{Name: "wiretest", Version: "0"},
	})
	require.Nil(t, resp.Error)

	resp, notes := client.call(libacp.MethodSessionNew, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.Nil(t, resp.Error)
	require.Empty(t, notes,
		"the downstream agent's available_commands_update must NOT precede the external session/new result "+
			"(a client drops updates for a session id it has not yet learned)")
	var newResp libacp.NewSessionResponse
	require.NoError(t, json.Unmarshal(resp.Result, &newResp))
	require.NotEmpty(t, newResp.SessionID)
	require.Equal(t, agentName, parseAgentMeta(newResp.Meta),
		"external session/new result must echo the contenox.agent attribution")

	// The relayed downstream menu must be the FIRST notification after the result.
	after := client.drainNotifications(1)
	require.Equal(t, libacp.MethodSessionUpdate, after[0].Method)
	var cmdNote libacp.SessionNotification
	require.NoError(t, json.Unmarshal(after[0].Params, &cmdNote))
	require.Equal(t, libacp.SessionUpdateAvailableCommands, cmdNote.Update.SessionUpdate,
		"the first notification after the external session/new result must be the relayed downstream command menu")
	require.Equal(t, newResp.SessionID, cmdNote.SessionID,
		"the relayed menu must be remapped onto the upstream session id")
	require.NotEmpty(t, cmdNote.Update.AvailableCommands,
		"the relayed menu must carry the downstream agent's advertised commands")
}

// TestLoopback_ExternalAgent_RelaysDownstreamCommandMenu proves, through the real
// client stack, that an external session's upstream client actually RECEIVES the
// downstream agent's slash-command menu (remapped onto the upstream session id).
// This is the delivery counterpart to the wire-ordering test above.
func TestLoopback_ExternalAgent_RelaysDownstreamCommandMenu(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	agentName := registerStubAgentInDB(t, h.tr.deps.DB, "claude-stub-menu",
		map[string]string{"ACP_STUB_ADVERTISE_COMMANDS": "1"})

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)

	// The external session/new emits no menu/usage/banner of its own; the only
	// update it produces is the re-emitted downstream command menu.
	updates := h.lc.drain(t, 1)
	require.Equal(t, libacp.SessionUpdateAvailableCommands, updates[0].Update.SessionUpdate,
		"the downstream agent's command menu must be relayed to the upstream client")
	require.Equal(t, newResp.SessionID, updates[0].SessionID,
		"a relayed downstream menu must be remapped onto the upstream session id")
	names := make(map[string]bool, len(updates[0].Update.AvailableCommands))
	for _, c := range updates[0].Update.AvailableCommands {
		names[c.Name] = true
	}
	require.True(t, names["review"] && names["explain"],
		"the relayed menu must carry the stub's deterministic advertised commands")
}

// TestLoopback_ExternalAgent_SessionNewCarriesDownstreamConfigOptions proves the
// keystone of the config-option pass-through: a downstream agent's OWN advertised
// config options (here the stub's deterministic "stub-verbosity" select, carried in
// its session/new response) reach the upstream client synchronously in the external
// session/new response, and externalDriver.ConfigOptions surfaces them. Contrast the
// non-advertising stub tests above, which still see an empty set — nothing is
// synthesized, the pass-through only carries what the downstream advertises.
func TestLoopback_ExternalAgent_SessionNewCarriesDownstreamConfigOptions(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	agentName := registerStubAgentInDB(t, h.tr.deps.DB, "claude-stub-cfg",
		map[string]string{"ACP_STUB_ADVERTISE_CONFIG_OPTIONS": "1"})

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)
	require.NotEmpty(t, newResp.ConfigOptions,
		"an external session/new response must carry the downstream agent's own advertised config options")
	verbosity := optionByID(t, newResp.ConfigOptions, "stub-verbosity")
	require.Equal(t, "select", verbosity.Type)
	require.Equal(t, "low", verbosity.CurrentValue,
		"the downstream agent's option value must be passed through as-is")
	require.True(t, configOptionHasValue(verbosity, "high"))

	// The driver surfaces the downstream set followed by contenox's own HITL policy
	// select — no chain-engine model/think/token selects folded in.
	h.tr.sessionMu.Lock()
	entry := h.tr.sessions[newResp.SessionID]
	h.tr.sessionMu.Unlock()
	opts := h.tr.sessionConfigOptions(ctx, entry)
	require.Len(t, opts, 2,
		"an external session advertises the downstream options plus contenox's own HITL policy select")
	require.Equal(t, "stub-verbosity", opts[0].ID, "the downstream agent's own option comes first")
	require.Equal(t, configIDHITLPolicy, opts[1].ID,
		"contenox's HITL policy select is appended after the downstream surface")
}

// TestLoopback_ExternalAgent_SetConfigOptionRoundTripsToDownstream proves an
// upstream session/set_config_option on an external session is forwarded to the
// downstream agent's session/set_config_option and its confirmed value round-trips:
// the upstream response reflects the new value (proving it went downstream and back,
// not mutated locally), and the downstream's confirming config_option_update is
// relayed up onto the upstream session id.
func TestLoopback_ExternalAgent_SetConfigOptionRoundTripsToDownstream(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	agentName := registerStubAgentInDB(t, h.tr.deps.DB, "claude-stub-cfg-set",
		map[string]string{"ACP_STUB_ADVERTISE_CONFIG_OPTIONS": "1"})

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)

	setResp, err := h.client.SetSessionConfigOption(ctx, libacp.SetSessionConfigOptionRequest{
		SessionID: newResp.SessionID,
		ConfigID:  "stub-verbosity",
		Value:     libacp.StringConfigValue("high"),
	})
	require.NoError(t, err)
	require.Equal(t, "high", optionByID(t, setResp.ConfigOptions, "stub-verbosity").CurrentValue,
		"the set_config_option response must carry the downstream agent's confirmed value")

	// The downstream agent's confirming config_option_update is relayed upstream,
	// remapped onto the upstream session id.
	updates := h.lc.drain(t, 1)
	require.Equal(t, libacp.SessionUpdateConfigOption, updates[0].Update.SessionUpdate)
	require.Equal(t, newResp.SessionID, updates[0].SessionID,
		"a relayed downstream config_option_update must be remapped onto the upstream session id")
	require.Equal(t, "high", optionByID(t, updates[0].Update.ConfigOptions, "stub-verbosity").CurrentValue)

	// A rejected value is the downstream's call to refuse, surfaced to the client.
	_, err = h.client.SetSessionConfigOption(ctx, libacp.SetSessionConfigOptionRequest{
		SessionID: newResp.SessionID,
		ConfigID:  "stub-verbosity",
		Value:     libacp.StringConfigValue("bogus"),
	})
	require.Error(t, err, "the downstream agent rejects an unknown value and the error surfaces upstream")
}

// TestLoopback_ExternalAgent_LazyRespawnPushesConfigOptions proves the reload path:
// after the downstream connection is torn down (as it is after a session/load, which
// deliberately does not resurrect the downstream), the first subsequent prompt lazily
// respawns it and pushes a config_option_update so the reloaded session regains the
// downstream agent's pickers. Close() reproduces the post-load state (fresh driver,
// no live handle) deterministically without the session/load replay machinery.
func TestLoopback_ExternalAgent_LazyRespawnPushesConfigOptions(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	agentName := registerStubAgentInDB(t, h.tr.deps.DB, "claude-stub-cfg-respawn",
		map[string]string{"ACP_STUB_ADVERTISE_CONFIG_OPTIONS": "1"})

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)

	// Tear the downstream down, mirroring the post-session/load state where the
	// entry is external but no downstream process is live yet.
	h.tr.sessionMu.Lock()
	ext := h.tr.sessions[newResp.SessionID].driver.(*externalDriver)
	h.tr.sessionMu.Unlock()
	require.NoError(t, ext.Close())

	promptResp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("hello again")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, promptResp.StopReason)

	// The respawn pushes a config_option_update (before the downstream turn's ack and
	// the post-turn session_info_update): the reloaded session's pickers are restored.
	updates := h.lc.drain(t, 3)
	var restored *libacp.SessionNotification
	for i := range updates {
		if updates[i].Update.SessionUpdate == libacp.SessionUpdateConfigOption {
			restored = &updates[i]
			break
		}
	}
	require.NotNil(t, restored, "a lazy respawn must push a config_option_update to restore the pickers")
	require.Equal(t, newResp.SessionID, restored.SessionID)
	require.Equal(t, "low", optionByID(t, restored.Update.ConfigOptions, "stub-verbosity").CurrentValue)
}

// TestE2E_Wire_ExternalAgent_ConfigOptionUpdateAfterNewSessionResult is the
// config-option counterpart to the command-menu wire-ordering test: a downstream
// agent that advertises its config options as a DEFERRED config_option_update after
// its own session/new (rather than in the response) must have that update relayed to
// the upstream client STRICTLY AFTER the external session/new result — never before,
// when the client cannot yet resolve the session id. It pins the pre-bind caching:
// acpsvc holds the update and re-emits it via markBound after the result.
func TestE2E_Wire_ExternalAgent_ConfigOptionUpdateAfterNewSessionResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "wire-external-cfg.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	defer db.Close()

	agentName := registerStubAgentInDB(t, db, "claude-stub-cfg-wire",
		map[string]string{"ACP_STUB_CONFIG_OPTIONS_AFTER_NEW": "1"})

	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentSide := &wirePipe{r: agentR, w: agentW}
	clientSide := &wirePipe{r: clientR, w: clientW}

	factory := New(Deps{
		Engine:      &enginesvc.Engine{},
		DB:          db,
		WorkspaceID: "wire-external-cfg-ws",
	})
	conn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		return factory(c)
	})
	runDone := make(chan error, 1)
	go func() { runDone <- conn.Run(ctx) }()
	defer func() {
		_ = clientSide.Close()
		select {
		case <-runDone:
		case <-time.After(2 * time.Second):
			t.Error("connection did not shut down")
		}
	}()

	client := &wireClient{t: t, rw: clientSide}

	resp, _ := client.call(libacp.MethodInitialize, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientInfo:      &libacp.Implementation{Name: "wiretest", Version: "0"},
	})
	require.Nil(t, resp.Error)

	resp, notes := client.call(libacp.MethodSessionNew, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.Nil(t, resp.Error)
	require.Empty(t, notes,
		"the downstream agent's config_option_update must NOT precede the external session/new result "+
			"(a client drops updates for a session id it has not yet learned)")
	var newResp libacp.NewSessionResponse
	require.NoError(t, json.Unmarshal(resp.Result, &newResp))
	require.NotEmpty(t, newResp.SessionID)

	// The relayed downstream config options must be the FIRST notification after the result.
	after := client.drainNotifications(1)
	require.Equal(t, libacp.MethodSessionUpdate, after[0].Method)
	var cfgNote libacp.SessionNotification
	require.NoError(t, json.Unmarshal(after[0].Params, &cfgNote))
	require.Equal(t, libacp.SessionUpdateConfigOption, cfgNote.Update.SessionUpdate,
		"the first notification after the external session/new result must be the relayed downstream config options")
	require.Equal(t, newResp.SessionID, cfgNote.SessionID,
		"the relayed config options must be remapped onto the upstream session id")
	require.NotEmpty(t, cfgNote.Update.ConfigOptions,
		"the relayed config_option_update must carry the downstream agent's advertised options")
}

// wireExternalConn is one live production Transport wired to a wireClient over an
// NDJSON pipe, for tests that need to reconnect a FRESH Transport against the same DB
// (reproducing a new process / new connection over a persisted session). shutdown
// tears the connection down; it is idempotent (safe to call manually then again from
// t.Cleanup).
type wireExternalConn struct {
	client   *wireClient
	shutdown func()
}

// dialWireTransport spins up a production Transport bound to the given DB and returns
// a wireClient talking to it plus an idempotent shutdown. The DB is owned by the
// caller (not closed here), so several connections can share one DB across a
// disconnect/reconnect. Mirrors the wire setup in TestE2E_Wire_* but reusable.
func dialWireTransport(ctx context.Context, t *testing.T, db libdb.DBManager, workspaceID string) *wireExternalConn {
	t.Helper()
	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	agentSide := &wirePipe{r: agentR, w: agentW}
	clientSide := &wirePipe{r: clientR, w: clientW}

	factory := New(Deps{
		Engine:      &enginesvc.Engine{},
		DB:          db,
		WorkspaceID: workspaceID,
	})
	conn := libacp.NewAgentSideConnection(agentSide, func(c *libacp.AgentSideConnection) libacp.Agent {
		return factory(c)
	})
	runDone := make(chan error, 1)
	go func() { runDone <- conn.Run(ctx) }()

	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			_ = clientSide.Close()
			select {
			case <-runDone:
			case <-time.After(3 * time.Second):
				t.Error("connection did not shut down")
			}
		})
	}
	t.Cleanup(shutdown)
	return &wireExternalConn{client: &wireClient{t: t, rw: clientSide}, shutdown: shutdown}
}

// drainForCommandMenu reads notifications after a response until an
// available_commands_update arrives (or the deadline), returning it. It tolerates
// unrelated notifications (e.g. a usage update) preceding the menu.
func drainForCommandMenu(t *testing.T, c *wireClient) libacp.SessionNotification {
	t.Helper()
	for i := 0; i < 8; i++ {
		note := c.drainNotifications(1)[0]
		require.Equal(t, libacp.MethodSessionUpdate, note.Method)
		var sn libacp.SessionNotification
		require.NoError(t, json.Unmarshal(note.Params, &sn))
		if sn.Update.SessionUpdate == libacp.SessionUpdateAvailableCommands {
			return sn
		}
	}
	t.Fatal("no available_commands_update arrived after the response")
	return libacp.SessionNotification{}
}

// TestE2E_Wire_ExternalAgent_ReloadRestoresMenuAndConfigOptions is the regression for
// the dropped downstream surface after a reconnect. An external session is created,
// its downstream agent advertises both a slash-command menu and config-option pickers,
// and a turn is driven; then the whole connection (and with it the downstream process)
// is torn down. A FRESH Transport on the SAME DB then session/loads the session with NO
// prompt, and must:
//   - carry the downstream config options in the session/load RESPONSE (restored from
//     persistence, since the downstream is not respawned during load), and
//   - re-emit the downstream command menu STRICTLY AFTER the load result, never before
//     it (a client drops updates for a session id it has not yet learned).
//
// Before this fix a reopened external session had no menu and no pickers until the
// first prompt lazily respawned the downstream. Driven at the raw wire so the
// notification-vs-response order is observable.
func TestE2E_Wire_ExternalAgent_ReloadRestoresMenuAndConfigOptions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "wire-external-reload.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	// Registered so it runs LAST (LIFO) — after every connection's shutdown Cleanup.
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	const ws = "wire-external-reload-ws"
	cwd := t.TempDir()
	agentName := registerStubAgentInDB(t, db, "claude-stub-reload", map[string]string{
		"ACP_STUB_ADVERTISE_COMMANDS":       "1",
		"ACP_STUB_ADVERTISE_CONFIG_OPTIONS": "1",
	})

	// --- Connection 1: create the external session, capture its advertised surface,
	// drive a turn, then drop the connection (killing the downstream process). ---
	c1 := dialWireTransport(ctx, t, db, ws)

	resp, _ := c1.client.call(libacp.MethodInitialize, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientInfo:      &libacp.Implementation{Name: "wiretest", Version: "0"},
	})
	require.Nil(t, resp.Error)

	resp, notes := c1.client.call(libacp.MethodSessionNew, libacp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.Nil(t, resp.Error)
	require.Empty(t, notes, "no update may precede the external session/new result")
	var newResp libacp.NewSessionResponse
	require.NoError(t, json.Unmarshal(resp.Result, &newResp))
	require.NotEmpty(t, newResp.SessionID)
	require.NotEmpty(t, newResp.ConfigOptions,
		"the external session/new response must carry the downstream config options")

	// Receiving the relayed menu upstream proves the bridge processed (and persisted)
	// the downstream available_commands_update.
	menu := drainForCommandMenu(t, c1.client)
	require.Equal(t, newResp.SessionID, menu.SessionID)

	// Drive a turn so the reloaded session also has history to replay (the menu
	// re-emit must coexist with replay).
	resp, _ = c1.client.call(libacp.MethodSessionPrompt, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("hello over the wire")},
	})
	require.Nil(t, resp.Error)

	c1.shutdown() // downstream process dies with the connection

	// --- Connection 2: fresh Transport, same DB. session/load with NO prompt. ---
	c2 := dialWireTransport(ctx, t, db, ws)

	resp, _ = c2.client.call(libacp.MethodInitialize, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientInfo:      &libacp.Implementation{Name: "wiretest", Version: "0"},
	})
	require.Nil(t, resp.Error)

	resp, notes = c2.client.call(libacp.MethodSessionLoad, libacp.LoadSessionRequest{
		SessionID:  newResp.SessionID,
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
	})
	require.Nil(t, resp.Error)

	// The load response restores the downstream config options from persistence —
	// without any prompt having respawned the downstream.
	var loadResp libacp.LoadSessionResponse
	require.NoError(t, json.Unmarshal(resp.Result, &loadResp))
	require.NotEmpty(t, loadResp.ConfigOptions,
		"session/load must restore the downstream config options from persistence, no prompt required")
	require.Equal(t, "stub-verbosity", optionByID(t, loadResp.ConfigOptions, "stub-verbosity").ID)

	// The command menu must NOT precede the load result (only replayed history may).
	for _, n := range notes {
		require.Equal(t, libacp.MethodSessionUpdate, n.Method, "only history replay precedes the load result")
		var sn libacp.SessionNotification
		require.NoError(t, json.Unmarshal(n.Params, &sn))
		require.NotEqual(t, libacp.SessionUpdateAvailableCommands, sn.Update.SessionUpdate,
			"the downstream command menu must NOT precede the load result")
	}

	// The persisted menu is re-emitted after the load result — restored with no prompt.
	reloaded := drainForCommandMenu(t, c2.client)
	require.Equal(t, newResp.SessionID, reloaded.SessionID,
		"the re-emitted menu must be remapped onto the upstream session id")
	names := make(map[string]bool, len(reloaded.Update.AvailableCommands))
	for _, cmd := range reloaded.Update.AvailableCommands {
		names[cmd.Name] = true
	}
	require.True(t, names["review"] && names["explain"],
		"the re-emitted menu must carry the downstream agent's advertised commands")
}

// TestLoopback_NativeSession_LoadUnaffectedByReloadPath is the (b) regression guard:
// the external reload path must not touch a NATIVE session. A native session/load still
// carries the chain-engine config options in its response and still emits the contenox
// slash-command menu after the result, and the loaded entry stays native.
func TestLoopback_NativeSession_LoadUnaffectedByReloadPath(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	cwd := t.TempDir()
	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{Cwd: cwd, McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	require.NotEmpty(t, newResp.ConfigOptions, "a native session/new advertises the chain config options")
	h.lc.drain(t, 1) // deferred available_commands_update after session/new

	loadResp, err := h.client.LoadSession(ctx, libacp.LoadSessionRequest{
		SessionID:  newResp.SessionID,
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)
	require.NotEmpty(t, loadResp.ConfigOptions,
		"a native session/load must still carry the chain config options (the external reload path must not intercept it)")

	got := h.lc.drain(t, 1)
	require.Equal(t, libacp.SessionUpdateAvailableCommands, got[0].Update.SessionUpdate,
		"a native session/load still emits the contenox slash-command menu")
	require.Equal(t, newResp.SessionID, got[0].SessionID)

	h.tr.sessionMu.Lock()
	entry := h.tr.sessions[newResp.SessionID]
	h.tr.sessionMu.Unlock()
	require.NotNil(t, entry)
	require.IsType(t, &nativeDriver{}, entry.driver, "a native session/load stays backed by a native driver")
}

// TestLoopback_ExternalAgent_SlashPromptPassesThroughVerbatim pins passthrough
// purity: a prompt beginning with "/" on an external session reaches the
// downstream agent as ordinary prompt text with ZERO contenox interception. It
// uses "/help now" — the name of a REAL contenox admin command that a NATIVE
// session intercepts (returning the help listing) — and asserts the reply is the
// downstream stub's plain "ack", never the contenox help output nor an "unknown
// command" error. The nativeDriver's slash handling (parseCommand/dispatchCommand)
// lives only in nativeDriver.Prompt; externalDriver.Prompt forwards the prompt
// blocks straight to the downstream session/prompt, so it is structurally
// unreachable for an external session — this proves it behaviorally.
func TestLoopback_ExternalAgent_SlashPromptPassesThroughVerbatim(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	agentName := registerStubAgent(t, h, "claude-stub-passthrough")

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)

	promptResp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("/help now")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, promptResp.StopReason,
		"a slash prompt on an external session ends via the downstream turn, not a contenox command")

	// The stub's plain-prompt path acks with one agent_message_chunk; the bridge
	// relays it up, plus the post-turn session_info_update.
	updates := h.lc.drain(t, 2)
	var acked bool
	for _, u := range updates {
		if u.Update.SessionUpdate == libacp.SessionUpdateAgentMessageChunk && u.Update.Content != nil {
			text := u.Update.Content.Text
			require.Equal(t, "ack", text,
				"the downstream stub's reply must be relayed verbatim, not a contenox command response")
			require.NotContains(t, text, "Available commands",
				"contenox must NOT intercept /help on an external session")
			require.NotContains(t, text, "unknown command",
				"contenox must NOT reject a slash prompt on an external session")
			acked = true
		}
	}
	require.True(t, acked, "the downstream agent's ack must be relayed upstream")
}

// TestLoopback_ExternalAgent_SessionNewCarriesSyntheticModeOption is the keystone of
// the mode pass-through: a downstream agent that advertises session Modes ONLY (here
// the stub opted into ACP_STUB_ADVERTISE_MODES, Code/Ask with Code current, and no
// config options of its own — the claude-code-acp shape) surfaces those modes as the
// single synthetic "Mode" select (id contenox.agent-mode) in the external session/new
// response, so the toolbar is no longer empty. The synthetic option leads the set,
// its value id/label come from each availableMode, and its currentValue mirrors the
// downstream currentModeId.
func TestLoopback_ExternalAgent_SessionNewCarriesSyntheticModeOption(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	agentName := registerStubAgentInDB(t, h.tr.deps.DB, "claude-stub-modes",
		map[string]string{"ACP_STUB_ADVERTISE_MODES": "1"})

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)
	require.NotEmpty(t, newResp.ConfigOptions,
		"an external session/new response must surface the downstream agent's modes as the synthetic mode select")
	require.Equal(t, AgentModeConfigOptionID, newResp.ConfigOptions[0].ID,
		"the synthetic mode select must lead the config-option set (mode first)")
	mode := optionByID(t, newResp.ConfigOptions, AgentModeConfigOptionID)
	require.Equal(t, "select", mode.Type)
	require.Equal(t, "Mode", mode.Name)
	require.Equal(t, "code", mode.CurrentValue,
		"the synthetic option's currentValue mirrors the downstream currentModeId")
	require.True(t, configOptionHasValue(mode, "code"))
	require.True(t, configOptionHasValue(mode, "ask"),
		"each downstream availableMode must be a selectable value")

	// The driver surfaces the synthetic mode option followed by contenox's own HITL
	// policy select — a modes-only downstream agent advertises no real config options,
	// and no chain-engine model/think/token selects are folded in.
	h.tr.sessionMu.Lock()
	entry := h.tr.sessions[newResp.SessionID]
	h.tr.sessionMu.Unlock()
	opts := h.tr.sessionConfigOptions(ctx, entry)
	require.Len(t, opts, 2,
		"a modes-only downstream agent surfaces the synthetic mode select plus contenox's own HITL policy select")
	require.Equal(t, AgentModeConfigOptionID, opts[0].ID, "the synthetic mode select leads")
	require.Equal(t, configIDHITLPolicy, opts[len(opts)-1].ID,
		"contenox's HITL policy select is appended last, after the downstream surface")
}

// TestLoopback_ExternalAgent_SetModeOptionRoundTripsToDownstream proves an upstream
// set_config_option on the synthetic mode id is translated to the downstream agent's
// session/set_mode, and the confirmed mode round-trips: the upstream response reflects
// the new mode (proving it went downstream, not mutated locally), and the downstream's
// confirming current_mode_update is relayed up (translated to a config_option_update)
// onto the upstream session id — receiving which proves the stub actually received
// session/set_mode.
func TestLoopback_ExternalAgent_SetModeOptionRoundTripsToDownstream(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	agentName := registerStubAgentInDB(t, h.tr.deps.DB, "claude-stub-modes-set",
		map[string]string{"ACP_STUB_ADVERTISE_MODES": "1"})

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)

	setResp, err := h.client.SetSessionConfigOption(ctx, libacp.SetSessionConfigOptionRequest{
		SessionID: newResp.SessionID,
		ConfigID:  AgentModeConfigOptionID,
		Value:     libacp.StringConfigValue("ask"),
	})
	require.NoError(t, err)
	require.Equal(t, "ask", optionByID(t, setResp.ConfigOptions, AgentModeConfigOptionID).CurrentValue,
		"the set_config_option response must carry the downstream agent's confirmed mode")

	// The stub's SetSessionMode emits a confirming current_mode_update, which contenox
	// translates and relays as a config_option_update onto the upstream session id.
	updates := h.lc.drain(t, 1)
	require.Equal(t, libacp.SessionUpdateConfigOption, updates[0].Update.SessionUpdate)
	require.Equal(t, newResp.SessionID, updates[0].SessionID,
		"a relayed downstream mode change must be remapped onto the upstream session id")
	require.Equal(t, "ask", optionByID(t, updates[0].Update.ConfigOptions, AgentModeConfigOptionID).CurrentValue)
}

// TestLoopback_ExternalAgent_CurrentModeUpdateRelaysAsConfigOption pins the relay
// TRANSLATION: a downstream current_mode_update is surfaced to the upstream client as
// a config_option_update over the synthetic mode id — never as a raw current_mode_update
// (contenox exposes no first-class client mode toggle on external sessions). Driven by a
// set on the synthetic id, which makes the stub emit the current_mode_update.
func TestLoopback_ExternalAgent_CurrentModeUpdateRelaysAsConfigOption(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	agentName := registerStubAgentInDB(t, h.tr.deps.DB, "claude-stub-modes-relay",
		map[string]string{"ACP_STUB_ADVERTISE_MODES": "1"})

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)

	_, err = h.client.SetSessionConfigOption(ctx, libacp.SetSessionConfigOptionRequest{
		SessionID: newResp.SessionID,
		ConfigID:  AgentModeConfigOptionID,
		Value:     libacp.StringConfigValue("ask"),
	})
	require.NoError(t, err)

	updates := h.lc.drain(t, 1)
	require.Equal(t, libacp.SessionUpdateConfigOption, updates[0].Update.SessionUpdate,
		"a downstream current_mode_update must surface as a config_option_update, not a raw mode update")
	require.Empty(t, updates[0].Update.CurrentModeID,
		"the translated update carries no raw currentModeId field — the mode rides the synthetic option")
	require.Equal(t, newResp.SessionID, updates[0].SessionID,
		"the translated update must be remapped onto the upstream session id")
	mode := optionByID(t, updates[0].Update.ConfigOptions, AgentModeConfigOptionID)
	require.Equal(t, "ask", mode.CurrentValue,
		"the translated config_option_update must carry the refreshed synthetic mode value")
	require.True(t, configOptionHasValue(mode, "code") && configOptionHasValue(mode, "ask"),
		"the refreshed synthetic option must still list every downstream mode")
}

// TestE2E_Wire_ExternalAgent_ReloadRestoresModePicker is the reload regression for the
// mode picker: an external session whose downstream advertises modes only is created,
// then the whole connection (and the downstream process) is torn down. A FRESH Transport
// on the SAME DB session/loads it with NO prompt and must restore the synthetic mode
// picker in the load RESPONSE — from persistence, since the downstream is not respawned
// during load. Before this the reopened toolbar lost the mode picker until the first
// prompt lazily respawned the downstream.
func TestE2E_Wire_ExternalAgent_ReloadRestoresModePicker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "wire-external-modes-reload.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	const ws = "wire-external-modes-reload-ws"
	cwd := t.TempDir()
	agentName := registerStubAgentInDB(t, db, "claude-stub-modes-reload",
		map[string]string{"ACP_STUB_ADVERTISE_MODES": "1"})

	// --- Connection 1: create the external session, then drop the connection. ---
	c1 := dialWireTransport(ctx, t, db, ws)
	resp, _ := c1.client.call(libacp.MethodInitialize, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientInfo:      &libacp.Implementation{Name: "wiretest", Version: "0"},
	})
	require.Nil(t, resp.Error)

	resp, notes := c1.client.call(libacp.MethodSessionNew, libacp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.Nil(t, resp.Error)
	require.Empty(t, notes, "no update may precede the external session/new result")
	var newResp libacp.NewSessionResponse
	require.NoError(t, json.Unmarshal(resp.Result, &newResp))
	require.NotEmpty(t, newResp.SessionID)
	require.Equal(t, AgentModeConfigOptionID, optionByID(t, newResp.ConfigOptions, AgentModeConfigOptionID).ID,
		"the external session/new response must carry the synthetic mode picker")

	c1.shutdown() // downstream process dies with the connection

	// --- Connection 2: fresh Transport, same DB. session/load with NO prompt. ---
	c2 := dialWireTransport(ctx, t, db, ws)
	resp, _ = c2.client.call(libacp.MethodInitialize, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientInfo:      &libacp.Implementation{Name: "wiretest", Version: "0"},
	})
	require.Nil(t, resp.Error)

	resp, _ = c2.client.call(libacp.MethodSessionLoad, libacp.LoadSessionRequest{
		SessionID:  newResp.SessionID,
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
	})
	require.Nil(t, resp.Error)

	var loadResp libacp.LoadSessionResponse
	require.NoError(t, json.Unmarshal(resp.Result, &loadResp))
	require.NotEmpty(t, loadResp.ConfigOptions,
		"session/load must restore the synthetic mode picker from persistence, no prompt required")
	mode := optionByID(t, loadResp.ConfigOptions, AgentModeConfigOptionID)
	require.Equal(t, "select", mode.Type)
	require.Equal(t, "code", mode.CurrentValue,
		"the restored mode picker must carry the persisted current mode")
	require.True(t, configOptionHasValue(mode, "code") && configOptionHasValue(mode, "ask"),
		"the restored mode picker must still list every downstream mode")
}

// TestLoopback_NativeSession_NoSyntheticModeOption is the (e) guard: the synthetic
// downstream-mode option is external-only. A native (chain-engine) session/new still
// advertises its chain selects and must NEVER carry the contenox.agent-mode option.
func TestLoopback_NativeSession_NoSyntheticModeOption(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)
	require.NotEmpty(t, newResp.ConfigOptions, "a native session still advertises chain config options")
	for _, o := range newResp.ConfigOptions {
		require.NotEqual(t, AgentModeConfigOptionID, o.ID,
			"the synthetic downstream-mode option is external-only; a native session must never carry it")
	}
}

// TestLoopback_ExternalAgent_SessionNewCarriesSyntheticModelOption is the model
// keystone: a downstream agent that advertises the UNSTABLE `models` state ONLY (here
// the stub opted into ACP_STUB_ADVERTISE_MODELS, Fast/Smart with Fast current, and no
// modes or config options of its own) surfaces those models as the single synthetic
// "Model" select (id contenox.agent-model) in the external session/new response. The
// synthetic option leads the downstream surface, its value id/label come from each
// availableModel, and its currentValue mirrors the downstream currentModelId.
func TestLoopback_ExternalAgent_SessionNewCarriesSyntheticModelOption(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	agentName := registerStubAgentInDB(t, h.tr.deps.DB, "claude-stub-models",
		map[string]string{"ACP_STUB_ADVERTISE_MODELS": "1"})

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)
	require.NotEmpty(t, newResp.ConfigOptions,
		"an external session/new response must surface the downstream agent's models as the synthetic model select")
	require.Equal(t, AgentModelConfigOptionID, newResp.ConfigOptions[0].ID,
		"the synthetic model select must lead the config-option set (no modes here, so model is first)")
	model := optionByID(t, newResp.ConfigOptions, AgentModelConfigOptionID)
	require.Equal(t, "select", model.Type)
	require.Equal(t, "Model", model.Name)
	require.Equal(t, "stub-model-fast", model.CurrentValue,
		"the synthetic option's currentValue mirrors the downstream currentModelId")
	require.True(t, configOptionHasValue(model, "stub-model-fast"))
	require.True(t, configOptionHasValue(model, "stub-model-smart"),
		"each downstream availableModel must be a selectable value")

	// The driver surfaces the synthetic model option followed by contenox's own HITL
	// policy select — a models-only downstream agent advertises no real config options.
	h.tr.sessionMu.Lock()
	entry := h.tr.sessions[newResp.SessionID]
	h.tr.sessionMu.Unlock()
	opts := h.tr.sessionConfigOptions(ctx, entry)
	require.Len(t, opts, 2,
		"a models-only downstream agent surfaces the synthetic model select plus contenox's own HITL policy select")
	require.Equal(t, AgentModelConfigOptionID, opts[0].ID, "the synthetic model select leads")
	require.Equal(t, configIDHITLPolicy, opts[len(opts)-1].ID,
		"contenox's HITL policy select is appended last, after the downstream surface")
}

// TestLoopback_ExternalAgent_SessionNewCarriesModeAndModelInOrder pins the full
// synthetic ordering: a downstream agent advertising BOTH session modes AND the
// UNSTABLE model picker (and no config options of its own) surfaces them in the fixed
// order mode, model, then contenox's own HITL policy select last — the toolbar order
// beam renders (mode, model, downstream options, hitl-policy).
func TestLoopback_ExternalAgent_SessionNewCarriesModeAndModelInOrder(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	agentName := registerStubAgentInDB(t, h.tr.deps.DB, "claude-stub-modes-models",
		map[string]string{"ACP_STUB_ADVERTISE_MODES": "1", "ACP_STUB_ADVERTISE_MODELS": "1"})

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)
	require.Len(t, newResp.ConfigOptions, 3,
		"a modes+models downstream agent surfaces the synthetic mode select, the synthetic model select, and contenox's HITL policy select")
	require.Equal(t, AgentModeConfigOptionID, newResp.ConfigOptions[0].ID,
		"the synthetic mode select leads")
	require.Equal(t, AgentModelConfigOptionID, newResp.ConfigOptions[1].ID,
		"the synthetic model select follows the mode select")
	require.Equal(t, configIDHITLPolicy, newResp.ConfigOptions[2].ID,
		"contenox's HITL policy select is last")
	require.Equal(t, "code", optionByID(t, newResp.ConfigOptions, AgentModeConfigOptionID).CurrentValue)
	require.Equal(t, "stub-model-fast", optionByID(t, newResp.ConfigOptions, AgentModelConfigOptionID).CurrentValue)
}

// TestLoopback_ExternalAgent_SetModelOptionRoundTripsToDownstream proves an upstream
// set_config_option on the synthetic model id is translated to the downstream agent's
// UNSTABLE session/set_model, and the confirmed model round-trips: the upstream response
// reflects the new model (proving it went downstream and was adopted, not mutated
// blindly). Unlike the mode path there is NO relayed update afterward — the ACP stream
// carries no model-update kind, so the stateless set_model response is the truth.
func TestLoopback_ExternalAgent_SetModelOptionRoundTripsToDownstream(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()
	agentName := registerStubAgentInDB(t, h.tr.deps.DB, "claude-stub-models-set",
		map[string]string{"ACP_STUB_ADVERTISE_MODELS": "1"})

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)

	setResp, err := h.client.SetSessionConfigOption(ctx, libacp.SetSessionConfigOptionRequest{
		SessionID: newResp.SessionID,
		ConfigID:  AgentModelConfigOptionID,
		Value:     libacp.StringConfigValue("stub-model-smart"),
	})
	require.NoError(t, err)
	require.Equal(t, "stub-model-smart", optionByID(t, setResp.ConfigOptions, AgentModelConfigOptionID).CurrentValue,
		"the set_config_option response must carry the downstream agent's confirmed model")
	require.True(t, configOptionHasValue(optionByID(t, setResp.ConfigOptions, AgentModelConfigOptionID), "stub-model-fast"),
		"the refreshed synthetic option must still list every downstream model")
}

// TestE2E_Wire_ExternalAgent_ReloadRestoresModelPicker is the reload regression for the
// model picker: an external session whose downstream advertises the UNSTABLE model state
// only is created, then the whole connection (and the downstream process) is torn down.
// A FRESH Transport on the SAME DB session/loads it with NO prompt and must restore the
// synthetic model picker in the load RESPONSE — from persistence, since the downstream
// is not respawned during load.
func TestE2E_Wire_ExternalAgent_ReloadRestoresModelPicker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "wire-external-models-reload.db"), runtimetypes.SchemaSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	const ws = "wire-external-models-reload-ws"
	cwd := t.TempDir()
	agentName := registerStubAgentInDB(t, db, "claude-stub-models-reload",
		map[string]string{"ACP_STUB_ADVERTISE_MODELS": "1"})

	// --- Connection 1: create the external session, then drop the connection. ---
	c1 := dialWireTransport(ctx, t, db, ws)
	resp, _ := c1.client.call(libacp.MethodInitialize, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientInfo:      &libacp.Implementation{Name: "wiretest", Version: "0"},
	})
	require.Nil(t, resp.Error)

	resp, notes := c1.client.call(libacp.MethodSessionNew, libacp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.Nil(t, resp.Error)
	require.Empty(t, notes, "no update may precede the external session/new result")
	var newResp libacp.NewSessionResponse
	require.NoError(t, json.Unmarshal(resp.Result, &newResp))
	require.NotEmpty(t, newResp.SessionID)
	require.Equal(t, AgentModelConfigOptionID, optionByID(t, newResp.ConfigOptions, AgentModelConfigOptionID).ID,
		"the external session/new response must carry the synthetic model picker")

	c1.shutdown() // downstream process dies with the connection

	// --- Connection 2: fresh Transport, same DB. session/load with NO prompt. ---
	c2 := dialWireTransport(ctx, t, db, ws)
	resp, _ = c2.client.call(libacp.MethodInitialize, libacp.InitializeRequest{
		ProtocolVersion: libacp.ProtocolVersion,
		ClientInfo:      &libacp.Implementation{Name: "wiretest", Version: "0"},
	})
	require.Nil(t, resp.Error)

	resp, _ = c2.client.call(libacp.MethodSessionLoad, libacp.LoadSessionRequest{
		SessionID:  newResp.SessionID,
		Cwd:        cwd,
		McpServers: []libacp.McpServer{},
	})
	require.Nil(t, resp.Error)

	var loadResp libacp.LoadSessionResponse
	require.NoError(t, json.Unmarshal(resp.Result, &loadResp))
	require.NotEmpty(t, loadResp.ConfigOptions,
		"session/load must restore the synthetic model picker from persistence, no prompt required")
	model := optionByID(t, loadResp.ConfigOptions, AgentModelConfigOptionID)
	require.Equal(t, "select", model.Type)
	require.Equal(t, "stub-model-fast", model.CurrentValue,
		"the restored model picker must carry the persisted current model")
	require.True(t, configOptionHasValue(model, "stub-model-fast") && configOptionHasValue(model, "stub-model-smart"),
		"the restored model picker must still list every downstream model")
}

// TestLoopback_NativeSession_NoSyntheticModelOption is the native guard: the synthetic
// downstream-model option is external-only. A native (chain-engine) session/new still
// advertises its chain selects and must NEVER carry the contenox.agent-model option.
func TestLoopback_NativeSession_NoSyntheticModelOption(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)
	require.NotEmpty(t, newResp.ConfigOptions, "a native session still advertises chain config options")
	for _, o := range newResp.ConfigOptions {
		require.NotEqual(t, AgentModelConfigOptionID, o.ID,
			"the synthetic downstream-model option is external-only; a native session must never carry it")
	}
}

// TestLoopback_ExternalAgent_HITLPolicyPickerRoundTripsNativelyAndPersists proves the
// external session's contenox-NATIVE HITL policy select: it is appended AFTER the
// downstream agent's surface, a set on it routes through the native per-session path
// (validated + stored on the session, resolved for enforcement, and NEVER forwarded to
// the downstream agent — which knows no such id), and the selection survives a
// session/load that rebuilds the entry with the sentinel default. This is the picker
// beam's file-explorer agent-view evaluates its HITL labels against, so exposing it —
// and keeping its value — is what fixes the labels for external sessions.
func TestLoopback_ExternalAgent_HITLPolicyPickerRoundTripsNativelyAndPersists(t *testing.T) {
	h := newLoopbackHarness(t)
	// The HITL policy select validates a concrete pick against the operator's known
	// presets. Set before any RPC reads them — this happens-before the agent
	// goroutine's read (a client call writes the request pipe, which synchronizes).
	h.tr.deps.KnownPolicies = []string{"strict", "dev"}
	h.tr.deps.HITLDefaultPolicyName = "strict"
	ctx := context.Background()
	agentName := registerStubAgentInDB(t, h.tr.deps.DB, "claude-stub-hitl",
		map[string]string{"ACP_STUB_ADVERTISE_CONFIG_OPTIONS": "1"})

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
		Meta:       agentMetaJSON(agentName),
	})
	require.NoError(t, err)
	// The HITL policy select rides AFTER the downstream agent's own surface.
	require.Equal(t, "stub-verbosity", newResp.ConfigOptions[0].ID, "the downstream option comes first")
	require.Equal(t, configIDHITLPolicy, newResp.ConfigOptions[len(newResp.ConfigOptions)-1].ID,
		"contenox's HITL policy select is appended last, after the downstream surface")
	require.Equal(t, hitlPolicyDefaultValue,
		optionByID(t, newResp.ConfigOptions, configIDHITLPolicy).CurrentValue,
		"a fresh external session defaults to the sentinel policy")

	// Setting the HITL policy routes through the NATIVE per-session path: stored on the
	// session and reflected in the response WITHOUT reaching the downstream stub (which
	// would reject an unknown "hitl-policy" id — the round trip succeeding proves it
	// never went downstream, and the downstream option below stays put).
	setResp, err := h.client.SetSessionConfigOption(ctx, libacp.SetSessionConfigOptionRequest{
		SessionID: newResp.SessionID,
		ConfigID:  configIDHITLPolicy,
		Value:     libacp.StringConfigValue("dev"),
	})
	require.NoError(t, err)
	require.Equal(t, "dev", optionByID(t, setResp.ConfigOptions, configIDHITLPolicy).CurrentValue,
		"the HITL policy set is reflected in the external session's config options")
	require.Equal(t, "low", optionByID(t, setResp.ConfigOptions, "stub-verbosity").CurrentValue,
		"the downstream option is untouched — the HITL set never went downstream")

	// It resolved through the native per-session enforcement path — what prompt.go and
	// the runtime-mediated (terminal bridge / future fs) gating read.
	h.tr.sessionMu.Lock()
	entry := h.tr.sessions[newResp.SessionID]
	h.tr.sessionMu.Unlock()
	require.Equal(t, "dev", entry.hitlPolicy(), "the selection is stored on the session")
	require.Equal(t, "dev", h.tr.resolveSessionHITLPolicy(entry),
		"the external session's HITL policy resolves to its own name for gating")

	// Reload: session/load rebuilds the entry with the sentinel default;
	// markExternalIfPersisted must restore the persisted selection and
	// reloadedConfigOptions must re-advertise the picker with the value intact.
	store := runtimetypes.New(h.tr.deps.DB.WithoutTransaction())
	reloaded := &sessionEntry{HITLPolicy: hitlPolicyDefaultValue, driver: &nativeDriver{t: h.tr}}
	h.tr.markExternalIfPersisted(ctx, store, newResp.SessionID, reloaded)
	require.IsType(t, &externalDriver{}, reloaded.driver, "the reloaded entry is re-flagged external")
	require.Equal(t, "dev", reloaded.hitlPolicy(), "the per-session HITL policy survives a reload")
	reloadedOpts := h.tr.reloadedConfigOptions(ctx, store, newResp.SessionID, reloaded)
	require.Equal(t, configIDHITLPolicy, reloadedOpts[len(reloadedOpts)-1].ID,
		"the reloaded external session re-advertises the HITL policy picker after the downstream surface")
	require.Equal(t, "dev", optionByID(t, reloadedOpts, configIDHITLPolicy).CurrentValue,
		"the reloaded picker shows the previously-chosen value, not the sentinel default")
}

// TestLoopback_NativeSession_PolicySlashCommandStillWorks is the /policy regression
// guard: the NATIVE slash command still switches the GLOBAL cli.hitl-policy-name KV
// (the operator-owned default the engine reads live) — distinct from the per-session
// toolbar HITL picker, which never writes that KV. External sessions instead pass
// "/policy" through to the downstream agent verbatim (see the passthrough test above);
// this pins that leaving that passthrough pure did not regress the native command.
func TestLoopback_NativeSession_PolicySlashCommandStillWorks(t *testing.T) {
	h := newLoopbackHarness(t)
	ctx := context.Background()

	_, err := h.client.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	newResp, err := h.client.NewSession(ctx, libacp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []libacp.McpServer{},
	})
	require.NoError(t, err)
	h.lc.drain(t, 1) // the deferred available_commands_update after session/new

	promptResp, err := h.client.Prompt(ctx, libacp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("/policy dev")},
	})
	require.NoError(t, err)
	require.Equal(t, libacp.StopReasonEndTurn, promptResp.StopReason,
		"a native /policy command resolves as an ended turn, not a downstream prompt")

	// dispatchCommand emits the command's confirmation as an agent_message_chunk and,
	// because /policy updates config options, a follow-up config_option_update.
	updates := h.lc.drain(t, 2)
	var confirmed bool
	for _, u := range updates {
		if u.Update.SessionUpdate == libacp.SessionUpdateAgentMessageChunk && u.Update.Content != nil {
			require.Contains(t, u.Update.Content.Text, "HITL policy set to dev",
				"the native /policy switch confirms inline")
			confirmed = true
		}
	}
	require.True(t, confirmed, "the /policy confirmation must reach the client")

	// The native slash path writes the GLOBAL KV (contrast the per-session toolbar
	// picker, which the test above proves never does).
	require.Equal(t, "dev", clikv.ReadHITLPolicy(ctx, runtimetypes.New(h.tr.deps.DB.WithoutTransaction())),
		"native /policy still writes the global cli.hitl-policy-name KV")
}
