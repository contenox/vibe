package approvalflow

import (
	"encoding/json"
	"testing"

	"github.com/contenox/beam/internal/services/hitlservice"
	"github.com/contenox/beam/libacp"
	"github.com/stretchr/testify/require"
)

// These tests pin what MapRequest refuses to invent as much as what it maps.

func TestUnit_MapRequest_RoundTripsBuildRequest(t *testing.T) {
	args := map[string]any{"path": "/workspace/main.go", "content": "hello"}
	built := BuildRequest(hitlservice.ApprovalRequest{
		ToolCallID: "call-7",
		ToolsName:  "local_fs",
		ToolName:   "write_file",
		Args:       args,
		Diff:       "--- a\n+++ b\n",
	}, BuildOptions{SessionID: "sess-1", PolicyName: "envelope.json"})

	mapped := MapRequest(built)
	require.True(t, mapped.Named)
	require.Equal(t, "local_fs", mapped.ToolsName)
	require.Equal(t, "write_file", mapped.ToolName)
	require.True(t, mapped.ArgsKnown)
	require.Equal(t, "/workspace/main.go", mapped.Args["path"])
	require.Equal(t, "envelope.json", mapped.PolicyName)
	require.Equal(t, "--- a\n+++ b\n", mapped.Diff)
	require.Equal(t, "call-7", mapped.ToolCallID)
	require.NotEmpty(t, mapped.Title)
}

// A foreign request with no contenox envelope is unnamed, not guessed from the title or tool-call id.
func TestUnit_MapRequest_ForeignRequestIsUnnamed(t *testing.T) {
	mapped := MapRequest(libacp.RequestPermissionRequest{
		SessionID: "sess-1",
		ToolCall: libacp.PermissionToolCall{
			ToolCallID: "local_fs.write_file",
			Title:      "Write file main.go",
			Kind:       libacp.ToolKindEdit,
			RawInput:   json.RawMessage(`{"path":"/workspace/main.go"}`),
		},
	})
	require.False(t, mapped.Named,
		"a tool-call id that merely LOOKS like tools.tool must not be mined for a policy identity")
	require.Empty(t, mapped.ToolsName)
	require.Empty(t, mapped.ToolName)
	require.True(t, mapped.ArgsKnown, "arguments are still recoverable when the identity is not")
	require.Equal(t, "Write file main.go", mapped.Title)
}

// Half an envelope (tools namespace without a tool, or the reverse) does not count as named.
func TestUnit_MapRequest_PartialMetaIsUnnamed(t *testing.T) {
	meta, err := json.Marshal(Meta{ToolsName: "local_fs"})
	require.NoError(t, err)

	mapped := MapRequest(libacp.RequestPermissionRequest{
		ToolCall: libacp.PermissionToolCall{ToolCallID: "c1", Meta: meta},
	})
	require.False(t, mapped.Named)
	require.Equal(t, "local_fs", mapped.ToolsName)
}

// Absent, malformed, or non-object arguments are reported unknown, never as an empty map.
func TestUnit_MapRequest_ArgsKnownOnlyForObjects(t *testing.T) {
	meta, err := json.Marshal(Meta{ToolsName: "local_fs", ToolName: "write_file"})
	require.NoError(t, err)

	for name, raw := range map[string]json.RawMessage{
		"absent":  nil,
		"array":   json.RawMessage(`["a","b"]`),
		"scalar":  json.RawMessage(`"just a string"`),
		"null":    json.RawMessage(`null`),
		"invalid": json.RawMessage(`{not json`),
	} {
		t.Run(name, func(t *testing.T) {
			mapped := MapRequest(libacp.RequestPermissionRequest{
				ToolCall: libacp.PermissionToolCall{ToolCallID: "c1", RawInput: raw, Meta: meta},
			})
			require.True(t, mapped.Named)
			require.False(t, mapped.ArgsKnown)
			require.NotNil(t, mapped.Args, "Args is always usable, even when unknown")
			require.Empty(t, mapped.Args)
		})
	}
}

// The request-level envelope is preferred over the tool-call one; either alone is enough.
func TestUnit_MapRequest_MetaFromEitherLevel(t *testing.T) {
	reqMeta, err := json.Marshal(Meta{ToolsName: "webtools", ToolName: "web_post"})
	require.NoError(t, err)
	callMeta, err := json.Marshal(Meta{ToolsName: "local_fs", ToolName: "sed"})
	require.NoError(t, err)

	both := MapRequest(libacp.RequestPermissionRequest{
		Meta:     reqMeta,
		ToolCall: libacp.PermissionToolCall{Meta: callMeta},
	})
	require.Equal(t, "webtools", both.ToolsName)
	require.Equal(t, "web_post", both.ToolName)

	callOnly := MapRequest(libacp.RequestPermissionRequest{
		ToolCall: libacp.PermissionToolCall{Meta: callMeta},
	})
	require.Equal(t, "local_fs", callOnly.ToolsName)
	require.Equal(t, "sed", callOnly.ToolName)
}

func TestUnit_Answer_PrefersOnceOverAlways(t *testing.T) {
	req := libacp.RequestPermissionRequest{Options: []libacp.PermissionOption{
		{OptionID: "always-yes", Kind: libacp.PermissionAllowAlways},
		{OptionID: "yes", Kind: libacp.PermissionAllowOnce},
		{OptionID: "always-no", Kind: libacp.PermissionRejectAlways},
		{OptionID: "no", Kind: libacp.PermissionRejectOnce},
	}}
	require.Equal(t, "yes", Answer(req, true).Outcome.OptionID)
	require.Equal(t, "no", Answer(req, false).Outcome.OptionID)
	require.Equal(t, libacp.PermissionOutcomeSelected, Answer(req, true).Outcome.Outcome)
}

func TestUnit_Answer_DegradesToCancelled(t *testing.T) {
	rejectOnly := libacp.RequestPermissionRequest{Options: []libacp.PermissionOption{
		{OptionID: "no", Kind: libacp.PermissionRejectOnce},
	}}
	granted := Answer(rejectOnly, true)
	require.Equal(t, libacp.PermissionOutcomeCancelled, granted.Outcome.Outcome)
	require.Empty(t, granted.Outcome.OptionID)

	none := Answer(libacp.RequestPermissionRequest{}, false)
	require.Equal(t, libacp.PermissionOutcomeCancelled, none.Outcome.Outcome)
}

func TestUnit_ParseMeta_EmptyIsAbsent(t *testing.T) {
	_, ok := ParseMeta(nil)
	require.False(t, ok)

	_, ok = ParseMeta(json.RawMessage(`{}`))
	require.False(t, ok)

	_, ok = ParseMeta(json.RawMessage(`{"nonsense":1}`))
	require.False(t, ok)

	meta, ok := ParseMeta(json.RawMessage(`{"toolsName":"echo"}`))
	require.True(t, ok)
	require.Equal(t, "echo", meta.ToolsName)
}
