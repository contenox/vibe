package vscodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	libdb "github.com/contenox/contenox/libdbexec"
	"github.com/contenox/contenox/libkvstore"
	"github.com/contenox/contenox/internal/services/agentservice"
	"github.com/contenox/contenox/internal/services/approvalflow"
	"github.com/contenox/contenox/internal/services/clikv"
	"github.com/contenox/contenox/internal/services/hitlservice"
	"github.com/contenox/contenox/internal/services/messagestore"
	"github.com/contenox/contenox/internal/store/runtimetypes"
	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/require"
)

type rpcTestResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *responseError  `json:"error,omitempty"`
}

type rpcTestNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func TestFramerRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, newFramer(bytes.NewReader(nil), &buf).writeMessage(map[string]any{
		"jsonrpc": "2.0",
		"id":      7,
		"result":  map[string]bool{"ok": true},
	}))

	payload, err := newFramer(bytes.NewReader(buf.Bytes()), io.Discard).readPayload()
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(payload, &got))
	require.Equal(t, "2.0", got["jsonrpc"])
	require.Equal(t, float64(7), got["id"])
}

func TestServerInitializeHealthAndShutdown(t *testing.T) {
	ctx, server, store := newTestServer(t)
	require.NoError(t, clikv.WriteConfig(ctx, store, "workspace-test", "default-provider", "openai"))
	require.NoError(t, clikv.WriteConfig(ctx, store, "workspace-test", "default-model", "gpt-5-mini"))

	responses := runServerRequests(t, ctx, server,
		rpcRequest(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "test-client"}}),
		rpcRequest(2, "health", nil),
		rpcRequest(3, "shutdown", nil),
	)
	require.Len(t, responses, 3)

	var initResult initializeResult
	require.NoError(t, json.Unmarshal(responses[0].Result, &initResult))
	require.Equal(t, ProtocolVersion, initResult.ProtocolVersion)
	require.Equal(t, "test-version", initResult.ServerVersion)
	require.Equal(t, "workspace-test", initResult.WorkspaceID)
	require.True(t, initResult.Capabilities.Config)
	require.False(t, initResult.Capabilities.Chat)
	require.True(t, initResult.Capabilities.Commands)
	require.Equal(t, "openai", initResult.Config.DefaultProvider)
	require.Equal(t, "gpt-5-mini", initResult.Config.DefaultModel)

	var health healthResult
	require.NoError(t, json.Unmarshal(responses[1].Result, &health))
	require.Equal(t, "ok", health.Status)
	require.True(t, health.Configured)
	require.Equal(t, "openai", health.DefaultProvider)
	require.Equal(t, "gpt-5-mini", health.DefaultModel)

	var shutdown map[string]bool
	require.NoError(t, json.Unmarshal(responses[2].Result, &shutdown))
	require.True(t, shutdown["ok"])
}

func TestServerRejectsUnknownParams(t *testing.T) {
	ctx, server, store := newTestServer(t)

	responses := runServerRequests(t, ctx, server,
		rpcRequest("bad", "setConfig", map[string]any{
			"defaultProvider": "openai",
			"surprise":        "do-not-ignore",
		}),
	)
	require.Len(t, responses, 1)
	require.NotNil(t, responses[0].Error)
	require.Equal(t, ErrInvalidParams, responses[0].Error.Code)

	got, _ := clikv.ReadConfig(ctx, store, "workspace-test", "default-provider")
	require.Empty(t, got)
}

func TestServerUnknownMethod(t *testing.T) {
	ctx, server, _ := newTestServer(t)

	responses := runServerRequests(t, ctx, server,
		rpcRequest(1, "definitelyMissing", nil),
	)
	require.Len(t, responses, 1)
	require.NotNil(t, responses[0].Error)
	require.Equal(t, ErrMethodNotFound, responses[0].Error.Code)
}

func TestSlashCommandsAdvertiseACPParitySet(t *testing.T) {
	cmds := slashCommands()
	names := make([]string, 0, len(cmds))
	for _, cmd := range cmds {
		names = append(names, cmd.Name)
		require.NotEmpty(t, cmd.Description)
	}
	require.ElementsMatch(t, []string{
		"help",
		"doctor",
		"clear",
		"compact",
		"model",
		"provider",
		"autocomplete-model",
		"autocomplete-provider",
		"max-tokens",
		"think",
		"capability",
		"policy",
		"websearch",
	}, names)
}

func TestParseSlashCommand(t *testing.T) {
	name, args, ok := parseSlashCommand("  /compact 12 ")
	require.True(t, ok)
	require.Equal(t, "compact", name)
	require.Equal(t, "12", args)

	_, _, ok = parseSlashCommand("/etc/passwd")
	require.False(t, ok)
}

func TestListCommandsRPC(t *testing.T) {
	ctx, server, _ := newTestServer(t)

	responses := runServerRequests(t, ctx, server, rpcRequest(1, "listCommands", nil))
	require.Len(t, responses, 1)
	require.Nil(t, responses[0].Error)
	var result listCommandsResult
	require.NoError(t, json.Unmarshal(responses[0].Result, &result))
	require.NotEmpty(t, result.Commands)
	require.Contains(t, commandNames(result.Commands), "compact")
	require.Contains(t, commandNames(result.Commands), "websearch")
}

func TestSessionTitleFromInput(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		fallback string
		want     string
	}{
		{
			name:  "plain prompt",
			input: "  Explain the new VS Code bridge lifecycle\nand setup flow  ",
			want:  "Explain the new VS Code bridge lifecycle and setup flow",
		},
		{
			name:  "mention and slash command",
			input: "@contenox /websearch    contenox vscode extension titles",
			want:  "Web search: contenox vscode extension titles",
		},
		{
			name:     "generic input uses fallback",
			input:    "vscode-chat",
			fallback: "New Contenox session",
			want:     "New Contenox session",
		},
		{
			name:     "new session from UI treated as generic",
			input:    "New session",
			fallback: "New Contenox session",
			want:     "New Contenox session",
		},
		{
			name:     "new session (2) from prior collision is generic",
			input:    "New session (3)",
			fallback: "New Contenox session",
			want:     "New Contenox session",
		},
		{
			name:  "empty command gets command title",
			input: "/doctor",
			want:  "Check Contenox setup",
		},
		{
			name:  "long title is shortened",
			input: "Summarize the workspace architecture and explain how session titles are generated for every editor integration",
			want:  "Summarize the workspace architecture and explain how session...",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, sessionTitleFromInput(tt.input, tt.fallback))
		})
	}
}

func TestSessionCreateUsesMeaningfulUniqueTitle(t *testing.T) {
	ctx, server, _ := newTestServer(t)

	first, err := server.sessionCreate(ctx, sessionCreateParams{Name: "/websearch contenox vscode extension"})
	require.NoError(t, err)
	require.Equal(t, "Web search: contenox vscode extension", first.Session.Name)

	second, err := server.sessionCreate(ctx, sessionCreateParams{Name: "/websearch contenox vscode extension"})
	require.NoError(t, err)
	require.Equal(t, "Web search: contenox vscode extension (2)", second.Session.Name)
}

func TestChatSendRenamesGenericSessionTitle(t *testing.T) {
	ctx, server, _ := newTestServer(t)

	id, err := server.sessionSvc().EnsureDefault(ctx, Identity)
	require.NoError(t, err)
	before, err := server.resolveSession(ctx, id, "")
	require.NoError(t, err)
	require.Equal(t, "default", before.Name)

	require.NoError(t, server.ensureMeaningfulSessionTitle(ctx, id, "Explain how Contenox session titles work"))
	after, err := server.resolveSession(ctx, id, "")
	require.NoError(t, err)
	require.Equal(t, "Explain how Contenox session titles work", after.Name)

	require.NoError(t, server.ensureMeaningfulSessionTitle(ctx, id, "A different prompt should not overwrite a real title"))
	still, err := server.resolveSession(ctx, id, "")
	require.NoError(t, err)
	require.Equal(t, "Explain how Contenox session titles work", still.Name)

	// Simulate creation from Beam/VSCode webview that sends the transient "New session"
	newID, err := server.sessionCreate(ctx, sessionCreateParams{Name: "New session"})
	require.NoError(t, err)
	require.Equal(t, "New Contenox session", newID.Session.Name, "creation with UI default should normalize to generic")

	require.NoError(t, server.ensureMeaningfulSessionTitle(ctx, newID.Session.ID, "How do I implement a rate limiter?"))
	renamed, err := server.resolveSession(ctx, newID.Session.ID, "")
	require.NoError(t, err)
	require.Equal(t, "How do I implement a rate limiter?", renamed.Name)
}

func TestSessionListOrdersByLatestMessageActivity(t *testing.T) {
	ctx, server, _ := newTestServer(t)

	older, err := server.sessionCreate(ctx, sessionCreateParams{Name: "Older session"})
	require.NoError(t, err)
	newer, err := server.sessionCreate(ctx, sessionCreateParams{Name: "Newer session"})
	require.NoError(t, err)

	store := messagestore.New(server.db.WithoutTransaction(), server.workspaceID)
	olderTime := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	newerTime := olderTime.Add(time.Hour)
	require.NoError(t, store.AppendMessages(ctx,
		storedMessage(t, older.Session.ID, "older-message", "older content", olderTime),
		storedMessage(t, newer.Session.ID, "newer-message", "newer content", newerTime),
	))
	require.NoError(t, server.sessionSvc().SetActiveID(ctx, older.Session.ID))

	listed, err := server.sessionList(ctx)
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 2)
	require.Equal(t, newer.Session.ID, listed.Sessions[0].ID)
	require.Equal(t, newerTime.Format(time.RFC3339), listed.Sessions[0].UpdatedAt)
	require.Equal(t, 1, listed.Sessions[0].MessageCount)
	require.False(t, listed.Sessions[0].IsActive)
	require.Equal(t, older.Session.ID, listed.Sessions[1].ID)
	require.True(t, listed.Sessions[1].IsActive)
}

func TestSessionReadDoesNotChangeActiveSession(t *testing.T) {
	ctx, server, _ := newTestServer(t)

	older, err := server.sessionCreate(ctx, sessionCreateParams{Name: "Older session"})
	require.NoError(t, err)
	newer, err := server.sessionCreate(ctx, sessionCreateParams{Name: "Newer session"})
	require.NoError(t, err)
	require.NoError(t, server.sessionSvc().SetActiveID(ctx, older.Session.ID))

	read, err := server.sessionRead(ctx, sessionReadParams{SessionID: newer.Session.ID})
	require.NoError(t, err)
	require.Equal(t, newer.Session.ID, read.Session.ID)

	listed, err := server.sessionList(ctx)
	require.NoError(t, err)
	requireSessionActiveState(t, listed.Sessions, older.Session.ID, true)
	requireSessionActiveState(t, listed.Sessions, newer.Session.ID, false)

	loaded, err := server.sessionLoad(ctx, sessionLoadParams{SessionID: newer.Session.ID})
	require.NoError(t, err)
	require.Equal(t, newer.Session.ID, loaded.Session.ID)

	listed, err = server.sessionList(ctx)
	require.NoError(t, err)
	requireSessionActiveState(t, listed.Sessions, older.Session.ID, false)
	requireSessionActiveState(t, listed.Sessions, newer.Session.ID, true)
}

func TestPublishTaskEventHidesRouteChunks(t *testing.T) {
	ctx, server, _ := newTestServer(t)
	var output bytes.Buffer
	server.framer = newFramer(bytes.NewReader(nil), &output)
	server.registerTurn("request-1", "turn-1", "session-1", func() {})
	defer server.unregisterTurn("request-1", "turn-1")

	server.publishTaskEvent(ctx, taskengine.TaskEvent{
		Kind:        taskengine.TaskEventStepChunk,
		RequestID:   "request-1",
		TaskHandler: taskengine.HandleRoute.String(),
		Content:     "general",
	})
	server.publishTaskEvent(ctx, taskengine.TaskEvent{
		Kind:        taskengine.TaskEventStepChunk,
		RequestID:   "request-1",
		TaskHandler: taskengine.HandleChatCompletion.String(),
		Content:     "Hello!",
	})

	notifications := decodeNotifications(t, output.Bytes())
	require.Len(t, notifications, 1)
	require.Equal(t, "chatDelta", notifications[0].Method)
	var delta chatDeltaEvent
	require.NoError(t, json.Unmarshal(notifications[0].Params, &delta))
	require.Equal(t, "session-1", delta.SessionID)
	require.Equal(t, "turn-1", delta.TurnID)
	require.Equal(t, "Hello!", delta.Content)
}

func TestPublishTaskEventForwardsHITLDecision(t *testing.T) {
	ctx, server, store := newTestServer(t)
	require.NoError(t, clikv.SetHITLPolicy(ctx, store, "hitl-policy-dev.json"))
	var output bytes.Buffer
	server.framer = newFramer(bytes.NewReader(nil), &output)
	server.registerTurn("request-1", "turn-1", "session-1", func() {})
	defer server.unregisterTurn("request-1", "turn-1")

	matchedRule := 3
	approvalRequested := false
	server.publishTaskEvent(ctx, taskengine.TaskEvent{
		Kind:                  taskengine.TaskEventHITLDecision,
		RequestID:             "request-1",
		HookName:              "local_shell",
		ToolName:              "local_shell",
		HITLAction:            "allow",
		HITLReason:            hitlservice.ReasonMatchedRule,
		HITLArgsSummary:       "python3",
		HITLMatchedRule:       &matchedRule,
		HITLApprovalRequested: &approvalRequested,
	})

	notifications := decodeNotifications(t, output.Bytes())
	require.Len(t, notifications, 1)
	require.Equal(t, "hitlDecision", notifications[0].Method)
	var event hitlDecisionEvent
	require.NoError(t, json.Unmarshal(notifications[0].Params, &event))
	require.Equal(t, "session-1", event.SessionID)
	require.Equal(t, "turn-1", event.TurnID)
	require.Equal(t, "local_shell", event.ToolsName)
	require.Equal(t, "local_shell", event.ToolName)
	require.Equal(t, "allow", event.Action)
	require.Equal(t, hitlservice.ReasonMatchedRule, event.Reason)
	require.Equal(t, "hitl-policy-dev.json", event.PolicyName)
	require.Equal(t, filepath.Join(server.stateDir, "hitl-policy-dev.json"), event.PolicyPath)
	require.Equal(t, "python3", event.ArgsSummary)
	require.Equal(t, &matchedRule, event.MatchedRule)
	require.False(t, event.ApprovalRequested)
}

func TestPublishTaskEventForwardsToolCallEvents(t *testing.T) {
	ctx, server, _ := newTestServer(t)
	var output bytes.Buffer
	server.framer = newFramer(bytes.NewReader(nil), &output)
	server.registerTurn("request-1", "turn-1", "session-1", func() {})
	defer server.unregisterTurn("request-1", "turn-1")

	server.publishTaskEvent(ctx, taskengine.TaskEvent{
		Kind:       taskengine.TaskEventToolCallPending,
		RequestID:  "request-1",
		ApprovalID: "call-1",
		ToolName:   "local_shell.local_shell",
		ApprovalArgs: map[string]any{
			"command": "python3",
		},
	})

	notifications := decodeNotifications(t, output.Bytes())
	require.Len(t, notifications, 1)
	require.Equal(t, "toolCall", notifications[0].Method)
}

func TestHandleClientResponsePayloadRoutesServerInitiatedResponse(t *testing.T) {
	_, server, _ := newTestServer(t)
	ch := make(chan clientResponse, 1)
	server.clientReqPending["7"] = ch

	handled, err := server.handleClientResponsePayload([]byte(`{"jsonrpc":"2.0","id":7,"result":{"ok":true}}`))
	require.NoError(t, err)
	require.True(t, handled)

	resp, ok := <-ch
	require.True(t, ok)
	require.JSONEq(t, `{"ok":true}`, string(resp.Result))
	require.Nil(t, resp.Error)
	_, stillPending := server.clientReqPending["7"]
	require.False(t, stillPending)
}

func TestWebSearchCommandUsesConfiguredEndpoint(t *testing.T) {
	ctx, server, _ := newTestServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "json", r.URL.Query().Get("format"))
		require.Equal(t, "contenox vscode", r.URL.Query().Get("q"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"Heading": "Contenox",
			"AbstractText": "Local runtime for AI workflows.",
			"AbstractURL": "https://example.com/contenox",
			"RelatedTopics": [
				{"Text": "VS Code extension - Native integration", "FirstURL": "https://example.com/vscode"}
			]
		}`))
	})
	httpServer := httptest.NewServer(mux)
	defer httpServer.Close()
	oldEndpoint := webSearchEndpoint
	webSearchEndpoint = httpServer.URL
	t.Cleanup(func() { webSearchEndpoint = oldEndpoint })

	out, err := server.commandWebSearch(ctx, "contenox vscode")
	require.NoError(t, err)
	require.Contains(t, out, "Web search results for \"contenox vscode\"")
	require.Contains(t, out, "[Contenox](https://example.com/contenox)")
	require.Contains(t, out, "[VS Code extension](https://example.com/vscode)")
}

func TestListMCPServersRPCSanitizesSecrets(t *testing.T) {
	ctx, server, store := newTestServer(t)
	require.NoError(t, store.CreateMCPServer(ctx, &runtimetypes.MCPServer{
		Name:       "filesystem",
		Transport:  "stdio",
		Command:    "npx",
		Args:       []string{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
		AuthType:   "bearer",
		AuthToken:  "literal-secret",
		AuthEnvKey: "MCP_TOKEN",
	}))

	responses := runServerRequests(t, ctx, server, rpcRequest(1, "listMCPServers", nil))
	require.Len(t, responses, 1)
	require.Nil(t, responses[0].Error)
	require.NotContains(t, string(responses[0].Result), "literal-secret")
	var result listMCPServersResult
	require.NoError(t, json.Unmarshal(responses[0].Result, &result))
	require.Len(t, result.Servers, 1)
	require.Equal(t, "filesystem", result.Servers[0].Name)
	require.Equal(t, "stdio", result.Servers[0].Transport)
	require.Equal(t, "npx", result.Servers[0].Command)
	require.Equal(t, "MCP_TOKEN", result.Servers[0].AuthEnvKey)
}

func TestCapabilitySetArgsAcceptsEqualsSyntax(t *testing.T) {
	provider, model, canThink, err := parseCapabilitySetArgs([]string{"set", "openai", "gpt-5-mini", "--think=true"})
	require.NoError(t, err)
	require.Equal(t, "openai", provider)
	require.Equal(t, "gpt-5-mini", model)
	require.True(t, canThink)
}

func TestThinkCommandIsSessionScoped(t *testing.T) {
	ctx, server, store := newTestServer(t)
	require.NoError(t, clikv.WriteConfig(ctx, store, "workspace-test", "default-think", "high"))

	out, err := server.commandThink(ctx, "session-a", "low")
	require.NoError(t, err)
	require.Equal(t, "Think set to low for this session.", out)

	require.Equal(t, "low", server.templateVars(ctx, "session-a")["think"])
	require.Equal(t, "high", server.templateVars(ctx, "session-b")["think"])
	got, _ := clikv.ReadConfig(ctx, store, "workspace-test", "default-think")
	require.Equal(t, "high", got)
}

func TestHITLPolicyConfigWritesLiveGlobalKey(t *testing.T) {
	ctx, server, store := newTestServer(t)
	policy := "hitl-policy-strict.json"

	cfg, err := server.setConfig(ctx, setConfigParams{HITLPolicyName: &policy})
	require.NoError(t, err)
	require.Equal(t, policy, cfg.HITLPolicyName)
	require.Equal(t, policy, clikv.ReadHITLPolicy(ctx, store))
}

func TestListHITLPoliciesIncludesPolicyFileMetadata(t *testing.T) {
	ctx, server, store := newTestServer(t)
	server.policyNames = []string{"hitl-policy-default.json", "hitl-policy-strict.json"}
	policy := "hitl-policy-strict.json"
	require.NoError(t, clikv.SetHITLPolicy(ctx, store, policy))

	result := server.listHitlPolicies(ctx)

	require.Equal(t, server.stateDir, result.PolicyDir)
	require.Equal(t, policy, result.ActivePolicyName)
	require.Equal(t, filepath.Join(server.stateDir, policy), result.ActivePolicyPath)
	require.Equal(t, []string{"hitl-policy-default.json", "hitl-policy-strict.json"}, result.Policies)
	require.Len(t, result.PolicyFiles, 2)
	require.Equal(t, hitlPolicyInfo{
		Name:   "hitl-policy-strict.json",
		Path:   filepath.Join(server.stateDir, "hitl-policy-strict.json"),
		Active: true,
	}, result.PolicyFiles[1])
}

func TestApprovalBrokerRequestsACPPermissionWithActivePolicyMetadata(t *testing.T) {
	ctx := context.Background()
	var got libacp.RequestPermissionRequest
	broker := NewApprovalBroker(
		func(_ context.Context, req libacp.RequestPermissionRequest) (libacp.RequestPermissionResponse, error) {
			got = req
			return libacp.RequestPermissionResponse{
				Outcome: libacp.RequestPermissionOutcome{
					Outcome:  libacp.PermissionOutcomeSelected,
					OptionID: approvalflow.OptionAllow,
				},
			}, nil
		},
		func(context.Context) hitlPolicyRef {
			return hitlPolicyRef{Name: "hitl-policy-strict.json", Path: "/tmp/contenox/hitl-policy-strict.json"}
		},
		func(context.Context) string { return "session-1" },
	)

	approved, err := broker.AskApproval(ctx, hitlservice.ApprovalRequest{
		ToolCallID: "call-1",
		ToolsName:  "local_fs",
		ToolName:   "write_file",
		Args:       map[string]any{"path": "README.md"},
	})

	require.NoError(t, err)
	require.True(t, approved)
	require.Equal(t, libacp.SessionID("session-1"), got.SessionID)
	require.Equal(t, "call-1", got.ToolCall.ToolCallID)
	require.Equal(t, "local_fs.write_file: README.md", got.ToolCall.Title)
	require.Equal(t, libacp.ToolKindEdit, got.ToolCall.Kind)
	require.JSONEq(t, `{"path":"README.md"}`, string(got.ToolCall.RawInput))
	var meta approvalflow.Meta
	require.NoError(t, json.Unmarshal(got.Meta, &meta))
	require.Equal(t, "local_fs", meta.ToolsName)
	require.Equal(t, "write_file", meta.ToolName)
	require.Equal(t, "hitl-policy-strict.json", meta.PolicyName)
	require.Equal(t, "/tmp/contenox/hitl-policy-strict.json", meta.PolicyPath)
}

func TestListModelsDoesNotInventProviderForObservedModels(t *testing.T) {
	ctx, server, store := newTestServer(t)
	require.NoError(t, store.AppendModel(ctx, &runtimetypes.Model{
		Model:         "qwen2.5:7b",
		ContextLength: 32768,
		CanChat:       true,
		CanPrompt:     true,
		CanStream:     true,
	}))
	require.NoError(t, clikv.WriteConfig(ctx, store, "workspace-test", "default-provider", "openai"))
	require.NoError(t, clikv.WriteConfig(ctx, store, "workspace-test", "default-model", "gpt-5-mini"))

	allResponses := runServerRequests(t, ctx, server, rpcRequest(1, "listModels", nil))
	require.Len(t, allResponses, 1)
	var all listModelsResult
	require.NoError(t, json.Unmarshal(allResponses[0].Result, &all))
	require.Len(t, all.Models, 2)
	requireModel(t, all.Models, "qwen2.5:7b", "", "observed")
	requireModel(t, all.Models, "gpt-5-mini", "openai", "config")

	filteredResponses := runServerRequests(t, ctx, server, rpcRequest(2, "listModels", map[string]string{"provider": "openai"}))
	require.Len(t, filteredResponses, 1)
	var filtered listModelsResult
	require.NoError(t, json.Unmarshal(filteredResponses[0].Result, &filtered))
	require.Len(t, filtered.Models, 2)
	requireModel(t, filtered.Models, "qwen2.5:7b", "", "observed")
	requireModel(t, filtered.Models, "gpt-5-mini", "openai", "config")
}

func TestAutocompleteCancelNotificationCancelsInFlightRequest(t *testing.T) {
	ctx, server, _ := newTestServer(t)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	server.buildRuntime = func(context.Context, RuntimeHooks) (*Runtime, error) {
		return &Runtime{
			Agent: &blockingAgent{
				started:   started,
				cancelled: cancelled,
			},
			FIMChain: &taskengine.TaskChainDefinition{ID: "fim-test"},
		}, nil
	}

	inputReader, inputWriter := io.Pipe()
	var output bytes.Buffer
	runDone := make(chan error, 1)
	go func() {
		runDone <- server.Run(ctx, inputReader, &output)
	}()
	writer := newFramer(bytes.NewReader(nil), inputWriter)
	require.NoError(t, writer.writeMessage(rpcRequest(9, "autocomplete", map[string]any{"prefix": "func main() {"})))
	require.Eventually(t, func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	require.NoError(t, writer.writeMessage(map[string]any{
		"jsonrpc": "2.0",
		"method":  "$/cancelRequest",
		"params":  map[string]any{"id": 9},
	}))
	require.Eventually(t, func() bool {
		select {
		case <-cancelled:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	require.NoError(t, writer.writeMessage(rpcRequest(10, "shutdown", nil)))
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-runDone)

	responses := decodeResponses(t, output.Bytes())
	require.NotNil(t, responseByID(responses, "9").Error)
	require.Equal(t, ErrInternal, responseByID(responses, "9").Error.Code)
	var shutdown map[string]bool
	require.NoError(t, json.Unmarshal(responseByID(responses, "10").Result, &shutdown))
	require.True(t, shutdown["ok"])
}

func TestCallClientSendsCancelNotificationWhenContextEnds(t *testing.T) {
	ctx, server, _ := newTestServer(t)
	var output bytes.Buffer
	server.framer = newFramer(bytes.NewReader(nil), &output)

	reqCtx, cancel := context.WithCancel(ctx)
	cancel()
	err := server.callClient(reqCtx, "session/request_permission", map[string]any{"sessionId": "session-1"}, nil)
	require.ErrorIs(t, err, context.Canceled)

	messages := decodeRawMessages(t, output.Bytes())
	require.Len(t, messages, 2)
	require.Equal(t, "session/request_permission", messages[0]["method"])
	require.Equal(t, "$/cancelRequest", messages[1]["method"])
	params, ok := messages[1]["params"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(1), params["id"])
}

func TestAutocompletePreservesLeadingWhitespace(t *testing.T) {
	ctx, server, _ := newTestServer(t)
	server.buildRuntime = func(context.Context, RuntimeHooks) (*Runtime, error) {
		return &Runtime{
			Agent:    &staticAgent{output: "\n\tvalue  \n"},
			FIMChain: &taskengine.TaskChainDefinition{ID: "fim-test"},
		}, nil
	}

	result, err := server.autocomplete(ctx, autocompleteParams{Prefix: "func main() {"})
	require.NoError(t, err)
	require.Equal(t, "\n\tvalue", result.Completion)
}

func TestAutocompletePrefersConfiguredCodeBackend(t *testing.T) {
	ctx, server, store := newTestServer(t)
	require.NoError(t, clikv.WriteConfig(ctx, store, "workspace-test", "default-provider", "openai"))
	require.NoError(t, clikv.WriteConfig(ctx, store, "workspace-test", "default-model", "gpt-5-mini"))
	require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
		Name:    "mistral",
		Type:    "mistral",
		BaseURL: "https://api.mistral.ai/v1",
	}))
	agent := &capturingAgent{output: "a + b"}
	server.buildRuntime = func(context.Context, RuntimeHooks) (*Runtime, error) {
		return &Runtime{
			Agent:    agent,
			FIMChain: &taskengine.TaskChainDefinition{ID: "fim-test"},
		}, nil
	}

	result, err := server.autocomplete(ctx, autocompleteParams{Prefix: "return ", Suffix: "\n}"})
	require.NoError(t, err)
	require.Equal(t, "a + b", result.Completion)
	require.Equal(t, "mistral", agent.lastReq.TemplateVars["provider"])
	require.Equal(t, "mistral-code-fim-latest", agent.lastReq.TemplateVars["model"])
}

func TestAutocompleteConfigOverridesCodeBackend(t *testing.T) {
	ctx, server, store := newTestServer(t)
	require.NoError(t, clikv.WriteConfig(ctx, store, "workspace-test", "default-provider", "openai"))
	require.NoError(t, clikv.WriteConfig(ctx, store, "workspace-test", "default-model", "gpt-5-mini"))
	require.NoError(t, clikv.WriteConfig(ctx, store, "workspace-test", "default-autocomplete-provider", "mistral"))
	require.NoError(t, clikv.WriteConfig(ctx, store, "workspace-test", "default-autocomplete-model", "codestral-latest"))
	require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
		Name:    "mistral",
		Type:    "mistral",
		BaseURL: "https://api.mistral.ai/v1",
	}))
	agent := &capturingAgent{output: "a + b"}
	server.buildRuntime = func(context.Context, RuntimeHooks) (*Runtime, error) {
		return &Runtime{
			Agent:    agent,
			FIMChain: &taskengine.TaskChainDefinition{ID: "fim-test"},
		}, nil
	}

	result, err := server.autocomplete(ctx, autocompleteParams{Prefix: "return ", Suffix: "\n}"})
	require.NoError(t, err)
	require.Equal(t, "a + b", result.Completion)
	require.Equal(t, "mistral", agent.lastReq.TemplateVars["provider"])
	require.Equal(t, "codestral-latest", agent.lastReq.TemplateVars["model"])
	require.Equal(t, "mistral", agent.lastReq.TemplateVars["autocomplete_provider"])
	require.Equal(t, "codestral-latest", agent.lastReq.TemplateVars["autocomplete_model"])
}

func TestAutocompleteExplicitProviderModelWins(t *testing.T) {
	ctx, server, store := newTestServer(t)
	require.NoError(t, store.CreateBackend(ctx, &runtimetypes.Backend{
		Name:    "mistral",
		Type:    "mistral",
		BaseURL: "https://api.mistral.ai/v1",
	}))
	agent := &capturingAgent{output: "a + b"}
	server.buildRuntime = func(context.Context, RuntimeHooks) (*Runtime, error) {
		return &Runtime{
			Agent:    agent,
			FIMChain: &taskengine.TaskChainDefinition{ID: "fim-test"},
		}, nil
	}

	_, err := server.autocomplete(ctx, autocompleteParams{
		Prefix:   "return ",
		Provider: "openai",
		Model:    "gpt-5-mini",
	})
	require.NoError(t, err)
	require.Equal(t, "openai", agent.lastReq.TemplateVars["provider"])
	require.Equal(t, "gpt-5-mini", agent.lastReq.TemplateVars["model"])
}

func newTestServer(t *testing.T) (context.Context, *Server, runtimetypes.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := libdb.NewSQLiteDBManager(ctx, filepath.Join(t.TempDir(), "vscodeagent.db"), runtimetypes.SchemaSQLite+"\n"+libkvstore.SQLiteSchema)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	server, err := New(ServerConfig{
		DB:          db,
		StateDir:    t.TempDir(),
		WorkspaceID: "workspace-test",
		Version:     "test-version",
	})
	require.NoError(t, err)
	return ctx, server, runtimetypes.New(db.WithoutTransaction())
}

func runServerRequests(t *testing.T, ctx context.Context, server *Server, requests ...map[string]any) []rpcTestResponse {
	t.Helper()
	var input bytes.Buffer
	writer := newFramer(bytes.NewReader(nil), &input)
	for _, req := range requests {
		require.NoError(t, writer.writeMessage(req))
	}

	var output bytes.Buffer
	require.NoError(t, server.Run(ctx, bytes.NewReader(input.Bytes()), &output))

	return decodeResponses(t, output.Bytes())
}

func decodeResponses(t *testing.T, raw []byte) []rpcTestResponse {
	t.Helper()
	reader := newFramer(bytes.NewReader(raw), io.Discard)
	responses := []rpcTestResponse{}
	for {
		payload, err := reader.readPayload()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		var resp rpcTestResponse
		require.NoError(t, json.Unmarshal(payload, &resp))
		responses = append(responses, resp)
	}
	return responses
}

func decodeNotifications(t *testing.T, raw []byte) []rpcTestNotification {
	t.Helper()
	reader := newFramer(bytes.NewReader(raw), io.Discard)
	notifications := []rpcTestNotification{}
	for {
		payload, err := reader.readPayload()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		var notification rpcTestNotification
		require.NoError(t, json.Unmarshal(payload, &notification))
		notifications = append(notifications, notification)
	}
	return notifications
}

func decodeRawMessages(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	reader := newFramer(bytes.NewReader(raw), io.Discard)
	messages := []map[string]any{}
	for {
		payload, err := reader.readPayload()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		var message map[string]any
		require.NoError(t, json.Unmarshal(payload, &message))
		messages = append(messages, message)
	}
	return messages
}

func rpcRequest(id any, method string, params any) map[string]any {
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	return req
}

func requireModel(t *testing.T, models []modelInfo, name, provider, source string) {
	t.Helper()
	for _, model := range models {
		if model.Name == name {
			require.Equal(t, provider, model.Provider)
			require.Equal(t, source, model.Source)
			return
		}
	}
	require.Failf(t, "model not found", "missing model %q in %+v", name, models)
}

func requireSessionActiveState(t *testing.T, sessions []sessionInfo, sessionID string, active bool) {
	t.Helper()
	for _, session := range sessions {
		if session.ID == sessionID {
			require.Equal(t, active, session.IsActive)
			return
		}
	}
	require.Failf(t, "session not found", "missing session %q in %+v", sessionID, sessions)
}

func commandNames(commands []slashCommand) []string {
	names := make([]string, 0, len(commands))
	for _, command := range commands {
		names = append(names, command.Name)
	}
	return names
}

func responseByID(responses []rpcTestResponse, id string) rpcTestResponse {
	for _, response := range responses {
		if string(response.ID) == id {
			return response
		}
	}
	return rpcTestResponse{}
}

func storedMessage(t *testing.T, sessionID, messageID, content string, addedAt time.Time) *messagestore.Message {
	t.Helper()
	payload, err := json.Marshal(taskengine.Message{
		ID:        messageID,
		Role:      "user",
		Content:   content,
		Timestamp: addedAt,
	})
	require.NoError(t, err)
	return &messagestore.Message{
		ID:      messageID,
		IDX:     sessionID,
		Payload: payload,
		AddedAt: addedAt,
	}
}

type blockingAgent struct {
	agentservice.Agent
	started   chan struct{}
	cancelled chan struct{}
}

func (a *blockingAgent) Prompt(ctx context.Context, _ agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
	close(a.started)
	<-ctx.Done()
	close(a.cancelled)
	return &agentservice.PromptResponse{StopReason: agentservice.StopCancelled}, ctx.Err()
}

type staticAgent struct {
	agentservice.Agent
	output string
}

func (a *staticAgent) Prompt(context.Context, agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
	return &agentservice.PromptResponse{Output: a.output, StopReason: agentservice.StopEndTurn}, nil
}

type capturingAgent struct {
	agentservice.Agent
	output  string
	lastReq agentservice.PromptRequest
}

func (a *capturingAgent) Prompt(_ context.Context, req agentservice.PromptRequest) (*agentservice.PromptResponse, error) {
	a.lastReq = req
	return &agentservice.PromptResponse{Output: a.output, StopReason: agentservice.StopEndTurn}, nil
}
