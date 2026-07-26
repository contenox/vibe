package libacp_test

import (
	"encoding/json"
	"testing"

	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The spec requires `used` and `size` on usage_update — zero values must reach
// the wire — while every other update kind must omit them.
func TestUnit_UsageUpdate_EmitsRequiredZeroFields(t *testing.T) {
	raw, err := json.Marshal(libacp.SessionUpdate{
		SessionUpdate: libacp.SessionUpdateUsageUpdate,
		Size:          4096,
	})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, float64(0), m["used"], "wire: %s", raw)
	assert.Equal(t, float64(4096), m["size"], "wire: %s", raw)

	chunk, err := json.Marshal(libacp.NewAgentMessageChunk("hi"))
	require.NoError(t, err)
	var cm map[string]any
	require.NoError(t, json.Unmarshal(chunk, &cm))
	_, hasUsed := cm["used"]
	_, hasSize := cm["size"]
	assert.False(t, hasUsed, "non-usage updates must not carry used: %s", chunk)
	assert.False(t, hasSize, "non-usage updates must not carry size: %s", chunk)
}

// `modes` in session/new responses is a SessionModeState object per spec, not
// a bare array of modes.
func TestUnit_SessionModes_WireShape(t *testing.T) {
	raw, err := json.Marshal(libacp.NewSessionResponse{
		SessionID: "s1",
		Modes: &libacp.SessionModeState{
			CurrentModeID: "ask",
			AvailableModes: []libacp.SessionMode{
				{ID: "ask", Name: "Ask"},
				{ID: "code", Name: "Code"},
			},
		},
	})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	modes, ok := m["modes"].(map[string]any)
	require.True(t, ok, "modes must be an object, wire: %s", raw)
	assert.Equal(t, "ask", modes["currentModeId"])
	avail, ok := modes["availableModes"].([]any)
	require.True(t, ok)
	assert.Len(t, avail, 2)
}

func TestUnit_AuthMethodEnvVar_WireShape(t *testing.T) {
	secretFalse := false
	raw, err := json.Marshal(libacp.AuthMethod{
		ID:   "env",
		Name: "Environment setup",
		Type: libacp.AuthMethodTypeEnvVar,
		Vars: []libacp.AuthEnvVar{
			{Name: "CONTENOX_DEFAULT_PROVIDER", Label: "Provider", Secret: &secretFalse},
			{Name: "OPENAI_API_KEY", Label: "OpenAI API key", Optional: true},
		},
	})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, "env_var", m["type"])
	vars, ok := m["vars"].([]any)
	require.True(t, ok)
	require.Len(t, vars, 2)
	first := vars[0].(map[string]any)
	assert.Equal(t, false, first["secret"], "explicit secret=false must reach the wire (spec default is true)")
	second := vars[1].(map[string]any)
	_, hasSecret := second["secret"]
	assert.False(t, hasSecret, "unset secret must be omitted so the spec default (true) applies")
	assert.Equal(t, true, second["optional"])
}

func TestUnit_ClientSessionCapabilities_RoundTrip(t *testing.T) {
	for wire, want := range map[string]bool{
		`{"fs":{},"session":{"configOptions":{"boolean":{}}}}`: true,
		`{"fs":{},"session":{"configOptions":{}}}`:             false,
		`{"fs":{}}`: false,
	} {
		var caps libacp.ClientCapabilities
		require.NoError(t, json.Unmarshal([]byte(wire), &caps))
		assert.Equal(t, want, caps.SupportsBooleanConfigOptions(), "wire: %s", wire)
	}
}
