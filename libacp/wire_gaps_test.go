package libacp_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/contenox/contenox/libacp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (a *testAgent) SetSessionMode(_ context.Context, req libacp.SetSessionModeRequest) (libacp.SetSessionModeResponse, error) {
	echo, err := json.Marshal(req)
	if err != nil {
		return libacp.SetSessionModeResponse{}, err
	}
	return libacp.SetSessionModeResponse{Meta: echo}, nil
}

func (a *testAgent) Logout(_ context.Context, _ libacp.LogoutRequest) (libacp.LogoutResponse, error) {
	return libacp.LogoutResponse{Meta: json.RawMessage(`{"seen":true}`)}, nil
}

func TestUnit_ClientSideConnection_SetSessionModeReachesAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agent := &testAgent{}
	client := &testClient{}
	_, clientConn, cleanup := wireUpTestConnections(t, ctx, agent, client)
	defer cleanup()

	_, err := clientConn.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)
	newSess, err := clientConn.NewSession(ctx, libacp.NewSessionRequest{Cwd: "/tmp", McpServers: []libacp.McpServer{}})
	require.NoError(t, err)

	resp, err := clientConn.SetSessionMode(ctx, libacp.SetSessionModeRequest{
		SessionID: newSess.SessionID,
		ModeID:    "code",
	})
	require.NoError(t, err)

	var echoed libacp.SetSessionModeRequest
	require.NoError(t, json.Unmarshal(resp.Meta, &echoed))
	assert.Equal(t, newSess.SessionID, echoed.SessionID)
	assert.Equal(t, "code", echoed.ModeID)
}

func TestUnit_ClientSideConnection_LogoutReachesAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agent := &testAgent{}
	client := &testClient{}
	_, clientConn, cleanup := wireUpTestConnections(t, ctx, agent, client)
	defer cleanup()

	_, err := clientConn.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	resp, err := clientConn.Logout(ctx, libacp.LogoutRequest{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"seen":true}`, string(resp.Meta))
}

type bareAgent struct {
	libacp.UnimplementedAgent
}

func (bareAgent) Initialize(_ context.Context, _ libacp.InitializeRequest) (libacp.InitializeResponse, error) {
	return libacp.InitializeResponse{ProtocolVersion: libacp.ProtocolVersion}, nil
}

func TestUnit_ClientSideConnection_SetSessionModeAndLogout_DefaultToMethodNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agentSide, clientSide := newPipePair()
	agentConn := libacp.NewAgentSideConnection(agentSide, func(*libacp.AgentSideConnection) libacp.Agent {
		return bareAgent{}
	})
	clientConn := libacp.NewClientSideConnection(clientSide, func(*libacp.ClientSideConnection) libacp.Client {
		return &testClient{}
	})

	agentRunErr := make(chan error, 1)
	go func() { agentRunErr <- agentConn.Run(ctx) }()
	clientRunErr := make(chan error, 1)
	go func() { clientRunErr <- clientConn.Run(ctx) }()
	defer func() {
		_ = agentSide.Close()
		select {
		case <-agentRunErr:
		case <-time.After(time.Second):
			t.Error("agent connection did not shut down")
		}
		select {
		case <-clientRunErr:
		case <-time.After(time.Second):
			t.Error("client connection did not shut down")
		}
	}()

	_, err := clientConn.Initialize(ctx, libacp.InitializeRequest{ProtocolVersion: libacp.ProtocolVersion})
	require.NoError(t, err)

	_, err = clientConn.SetSessionMode(ctx, libacp.SetSessionModeRequest{SessionID: "s1", ModeID: "code"})
	require.Error(t, err)
	var rpcErr *libacp.Error
	require.ErrorAs(t, err, &rpcErr)
	assert.Equal(t, libacp.ErrMethodNotFound, rpcErr.Code)

	_, err = clientConn.Logout(ctx, libacp.LogoutRequest{})
	require.Error(t, err)
	require.ErrorAs(t, err, &rpcErr)
	assert.Equal(t, libacp.ErrMethodNotFound, rpcErr.Code)
}

func TestUnit_SetSessionModeRequest_WireShape(t *testing.T) {
	raw, err := json.Marshal(libacp.SetSessionModeRequest{SessionID: "s1", ModeID: "ask"})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, "s1", m["sessionId"])
	assert.Equal(t, "ask", m["modeId"])
	_, hasMeta := m["_meta"]
	assert.False(t, hasMeta, "unset _meta must be omitted")
}

func TestUnit_LogoutRequestResponse_WireShape(t *testing.T) {
	raw, err := json.Marshal(libacp.LogoutRequest{})
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(raw))

	raw, err = json.Marshal(libacp.LogoutResponse{})
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(raw))
}

// Pins: a "boolean" SessionConfigOption puts a real JSON boolean on the wire
// (never "true") and carries no options key.
func TestUnit_SessionConfigOption_BooleanWireShape(t *testing.T) {
	raw, err := json.Marshal(libacp.SessionConfigOption{
		ID:           "auto-approve",
		Name:         "Auto-approve",
		Type:         libacp.SessionConfigOptionTypeBoolean,
		CurrentValue: "true",
	})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, true, m["currentValue"], "wire: %s", raw)
	_, hasOptions := m["options"]
	assert.False(t, hasOptions, "boolean options must not carry an options key, wire: %s", raw)

	var back libacp.SessionConfigOption
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, "true", back.CurrentValue, "boolean currentValue round-trips to the Go-side string form")
}

// Pins: "select" currentValue stays a string, and options is always present.
func TestUnit_SessionConfigOption_SelectWireShape(t *testing.T) {
	raw, err := json.Marshal(libacp.SessionConfigOption{
		ID:           "model",
		Name:         "Model",
		Type:         libacp.SessionConfigOptionTypeSelect,
		CurrentValue: "gpt-5",
		Options:      libacp.NewSessionConfigValues(nil),
	})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, "gpt-5", m["currentValue"], "wire: %s", raw)
	options, ok := m["options"].([]any)
	require.True(t, ok, "select options must always be present, wire: %s", raw)
	assert.Empty(t, options)

	var back libacp.SessionConfigOption
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, "gpt-5", back.CurrentValue)
}

// Pins: McpServerHttp/Sse always carry `headers` (even `[]`), and
// McpServerStdio always carries `args`/`env` — omitting them can break a
// strict receiver.
func TestUnit_McpServer_RequiredArraysAlwaysPresent(t *testing.T) {
	stdio, err := json.Marshal(libacp.McpServer{Name: "local", Command: "/bin/mcp"})
	require.NoError(t, err)
	var stdioM map[string]any
	require.NoError(t, json.Unmarshal(stdio, &stdioM))
	args, ok := stdioM["args"].([]any)
	require.True(t, ok, "wire: %s", stdio)
	assert.Empty(t, args)
	env, ok := stdioM["env"].([]any)
	require.True(t, ok, "wire: %s", stdio)
	assert.Empty(t, env)
	_, hasHeaders := stdioM["headers"]
	assert.False(t, hasHeaders, "stdio must not carry a headers key, wire: %s", stdio)

	http, err := json.Marshal(libacp.McpServer{Type: "http", Name: "remote", URL: "https://example.test"})
	require.NoError(t, err)
	var httpM map[string]any
	require.NoError(t, json.Unmarshal(http, &httpM))
	headers, ok := httpM["headers"].([]any)
	require.True(t, ok, "wire: %s", http)
	assert.Empty(t, headers)
	_, hasArgs := httpM["args"]
	assert.False(t, hasArgs, "http must not carry an args key, wire: %s", http)

	sse, err := json.Marshal(libacp.McpServer{Type: "sse", Name: "remote", URL: "https://example.test/sse"})
	require.NoError(t, err)
	var sseM map[string]any
	require.NoError(t, json.Unmarshal(sse, &sseM))
	sseHeaders, ok := sseM["headers"].([]any)
	require.True(t, ok, "wire: %s", sse)
	assert.Empty(t, sseHeaders)
}

// Pins: available_commands_update/config_option_update always carry their
// payload array, even empty; every other update kind omits it.
func TestUnit_SessionUpdate_AvailableCommandsAndConfigOptions_EmitEmptyArrays(t *testing.T) {
	raw, err := json.Marshal(libacp.SessionUpdate{SessionUpdate: libacp.SessionUpdateAvailableCommands})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	commands, ok := m["availableCommands"].([]any)
	require.True(t, ok, "available_commands_update must always carry availableCommands, wire: %s", raw)
	assert.Empty(t, commands)

	raw, err = json.Marshal(libacp.SessionUpdate{SessionUpdate: libacp.SessionUpdateConfigOption})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &m))
	options, ok := m["configOptions"].([]any)
	require.True(t, ok, "config_option_update must always carry configOptions, wire: %s", raw)
	assert.Empty(t, options)

	chunk, err := json.Marshal(libacp.NewAgentMessageChunk("hi"))
	require.NoError(t, err)
	var cm map[string]any
	require.NoError(t, json.Unmarshal(chunk, &cm))
	_, hasCommands := cm["availableCommands"]
	_, hasOptions := cm["configOptions"]
	assert.False(t, hasCommands, "non-matching updates must not carry availableCommands: %s", chunk)
	assert.False(t, hasOptions, "non-matching updates must not carry configOptions: %s", chunk)
}

func TestUnit_AdditionalDirectories_WireShape(t *testing.T) {
	newReq, err := json.Marshal(libacp.NewSessionRequest{
		Cwd:                   "/repo",
		AdditionalDirectories: []string{"/repo/vendor"},
		McpServers:            []libacp.McpServer{},
	})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(newReq, &m))
	dirs, ok := m["additionalDirectories"].([]any)
	require.True(t, ok, "wire: %s", newReq)
	assert.Equal(t, []any{"/repo/vendor"}, dirs)

	var back libacp.NewSessionRequest
	require.NoError(t, json.Unmarshal(newReq, &back))
	assert.Equal(t, []string{"/repo/vendor"}, back.AdditionalDirectories)

	// Omitted when unset, on every request that carries it and on SessionInfo.
	noDirs, err := json.Marshal(libacp.NewSessionRequest{Cwd: "/repo", McpServers: []libacp.McpServer{}})
	require.NoError(t, err)
	var noDirsM map[string]any
	require.NoError(t, json.Unmarshal(noDirs, &noDirsM))
	_, has := noDirsM["additionalDirectories"]
	assert.False(t, has, "wire: %s", noDirs)

	info, err := json.Marshal(libacp.SessionInfo{SessionID: "s1", Cwd: "/repo"})
	require.NoError(t, err)
	var infoM map[string]any
	require.NoError(t, json.Unmarshal(info, &infoM))
	_, has = infoM["additionalDirectories"]
	assert.False(t, has, "wire: %s", info)
}

func TestUnit_AgentAuthCapabilities_LogoutWireShape(t *testing.T) {
	raw, err := json.Marshal(libacp.AgentCapabilities{
		Auth: libacp.AgentAuthCapabilities{Logout: &libacp.LogoutCapabilities{}},
	})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	auth, ok := m["auth"].(map[string]any)
	require.True(t, ok, "wire: %s", raw)
	logout, ok := auth["logout"].(map[string]any)
	require.True(t, ok, "logout capability must be an object ({} means supported), wire: %s", raw)
	assert.Empty(t, logout)

	var back libacp.AgentCapabilities
	require.NoError(t, json.Unmarshal(raw, &back))
	require.NotNil(t, back.Auth.Logout)
}

func TestUnit_SessionCapabilities_AdditionalDirectoriesWireShape(t *testing.T) {
	raw, err := json.Marshal(libacp.SessionCapabilities{AdditionalDirectories: &struct{}{}})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	_, ok := m["additionalDirectories"].(map[string]any)
	assert.True(t, ok, "wire: %s", raw)
}

func TestUnit_Annotations_LastModifiedWireShape(t *testing.T) {
	raw, err := json.Marshal(libacp.Annotations{LastModified: "2024-01-01T00:00:00Z"})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, "2024-01-01T00:00:00Z", m["lastModified"])

	var back libacp.Annotations
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, "2024-01-01T00:00:00Z", back.LastModified)
}

// Pins: a diff clearing content to "" still puts path/newText on the wire
// for the "diff" tool-call-content variant.
func TestUnit_ToolCallContentDiff_EmitsEmptyNewText(t *testing.T) {
	raw, err := json.Marshal(libacp.ToolCallContent{
		Type:    libacp.ToolCallContentDiff,
		Path:    "/repo/emptied.txt",
		OldText: "was not empty",
		NewText: "",
	})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, "/repo/emptied.txt", m["path"])
	newText, ok := m["newText"]
	require.True(t, ok, "diff must carry newText even when empty, wire: %s", raw)
	assert.Equal(t, "", newText)

	// Non-diff kinds are unaffected: path/newText stay omitted when unset.
	regular, err := json.Marshal(libacp.ToolCallContent{
		Type:    libacp.ToolCallContentRegular,
		Content: &libacp.ContentBlock{Type: string(libacp.ContentKindText), Text: "ok"},
	})
	require.NoError(t, err)
	var rm map[string]any
	require.NoError(t, json.Unmarshal(regular, &rm))
	_, hasPath := rm["path"]
	_, hasNewText := rm["newText"]
	assert.False(t, hasPath, "wire: %s", regular)
	assert.False(t, hasNewText, "wire: %s", regular)
}

func TestUnit_EmbeddedResource_MetaRoundTrip(t *testing.T) {
	raw, err := json.Marshal(libacp.EmbeddedResource{
		URI:  "file:///a.txt",
		Text: "hi",
		Meta: json.RawMessage(`{"k":"v"}`),
	})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	meta, ok := m["_meta"].(map[string]any)
	require.True(t, ok, "wire: %s", raw)
	assert.Equal(t, "v", meta["k"])

	var back libacp.EmbeddedResource
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.JSONEq(t, `{"k":"v"}`, string(back.Meta))
}

func TestUnit_CapabilityStructs_MetaRoundTrip(t *testing.T) {
	meta := json.RawMessage(`{"k":"v"}`)

	for name, raw := range map[string]json.RawMessage{
		"FileSystemCapabilities": mustMarshal(t, libacp.FileSystemCapabilities{Meta: meta}),
		"PromptCapabilities":     mustMarshal(t, libacp.PromptCapabilities{Meta: meta}),
		"McpCapabilities":        mustMarshal(t, libacp.McpCapabilities{Meta: meta}),
	} {
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m), name)
		got, ok := m["_meta"].(map[string]any)
		require.True(t, ok, "%s wire: %s", name, raw)
		assert.Equal(t, "v", got["k"], name)
	}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

func TestUnit_ToolKind_SwitchModeConstant(t *testing.T) {
	assert.Equal(t, libacp.ToolKind("switch_mode"), libacp.ToolKindSwitchMode)
}

func TestUnit_MethodConstants_SetModeAndLogout(t *testing.T) {
	assert.Equal(t, "session/set_mode", libacp.MethodSessionSetMode)
	assert.Equal(t, "logout", libacp.MethodLogout)
}
