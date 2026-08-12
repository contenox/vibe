package taskengine

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fullyPopulatedMessage() Message {
	return Message{
		ID:      "msg-1",
		Role:    "assistant",
		Content: "content-1",
		Images: []ImagePart{
			{Data: []byte{0x01, 0x02, 0xFF}, MimeType: "image/png"},
		},
		Audio: []AudioPart{
			{Data: []byte{0x03, 0x04, 0xFE}, MimeType: "audio/wav"},
		},
		Thinking:   "thinking-1",
		ToolCallID: "call-prev",
		CallTools: []ToolCall{
			{
				ID:   "call-1",
				Type: "function",
				Function: FunctionCall{
					Name:      "local_fs.write_file",
					Arguments: `{"path":"/tmp/x","content":"y"}`,
				},
				ProviderMeta: map[string]string{"thought_signature": "sig-1"},
			},
		},
		Timestamp: time.Date(2026, 7, 27, 10, 30, 0, 0, time.UTC),
		RequestID: "req-1",
		ChainRef:  "chain-agent-contenox.json",
	}
}

func fullyPopulatedCheckpoint(t *testing.T) *Checkpoint {
	t.Helper()
	history := ChatHistory{
		Messages:     []Message{fullyPopulatedMessage()},
		Model:        "test-model",
		InputTokens:  11,
		OutputTokens: 7,
	}
	return &Checkpoint{
		ApprovalID: "call-1",
		PendingCalls: []PendingToolCall{
			{CallID: "call-1", Name: "local_fs.write_file", Arguments: `{"path":"/tmp/x"}`},
		},
		Chain: &TaskChainDefinition{
			ID: "chain-1",
			Tasks: []TaskDefinition{
				{ID: "exec", Handler: HandleExecuteToolCalls},
			},
			TokenLimit: 4096,
		},
		TaskID:     "exec",
		RetryIndex: 2,
		Scope:      EventScope{Chain: "chain-1", Task: "exec", ToolCall: "call-1"},
		Vars: map[string]any{
			"input":    history,
			"label":    "route-a",
			"count":    3,
			"blob":     map[string]any{"k": "v"},
			"anything": "free",
			"nothing":  nil,
		},
		VarTypes: map[string]DataType{
			"input":    DataTypeChatHistory,
			"label":    DataTypeString,
			"count":    DataTypeInt,
			"blob":     DataTypeJSON,
			"anything": DataTypeAny,
			"nothing":  DataTypeNil,
		},
		EdgeCounts:        map[string]int{"chat->exec": 4},
		History:           history,
		TemplateVars:      map[string]string{"chain": "chain-1", "think": "high"},
		ToolsAllowlist:    []string{"local_fs", "!local_shell"},
		HasToolsAllowlist: true,
		ContextLength:     8192,
		SessionID:         "sess-1",
		MissionID:         "mission-1",
		RequestID:         "req-1",
		ChainRef:          "chain-agent-contenox.json",
		CreatedAt:         time.Date(2026, 7, 27, 10, 31, 0, 0, time.UTC),
	}
}

func requireNoZeroFields(t *testing.T, name string, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			require.False(t, v.Interface().(time.Time).IsZero(), "%s must be non-zero in the fixture", name)
			return
		}
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			requireNoZeroFields(t, name+"."+f.Name, v.Field(i))
		}
	case reflect.Slice, reflect.Map:
		require.Positive(t, v.Len(), "%s must be non-empty in the fixture", name)
		for i := 0; i < v.Len() && v.Kind() == reflect.Slice; i++ {
			requireNoZeroFields(t, fmt.Sprintf("%s[%d]", name, i), v.Index(i))
		}
	default:
		require.False(t, v.IsZero(), "%s must be non-zero in the fixture — a zero fixture value cannot prove the field round-trips", name)
	}
}

// TestUnit_Checkpoint_FixtureCoversEveryMessageField fails when a new Message
// field is added without extending fullyPopulatedMessage.
func TestUnit_Checkpoint_FixtureCoversEveryMessageField(t *testing.T) {
	requireNoZeroFields(t, "Message", reflect.ValueOf(fullyPopulatedMessage()))
}

// TestUnit_Checkpoint_RoundTrip_EveryField marshals a fully populated
// checkpoint and asserts the decoded result is field-for-field identical.
func TestUnit_Checkpoint_RoundTrip_EveryField(t *testing.T) {
	cp := fullyPopulatedCheckpoint(t)
	raw, err := MarshalCheckpoint(cp)
	require.NoError(t, err)

	got, err := UnmarshalCheckpoint(raw)
	require.NoError(t, err)

	require.IsType(t, ChatHistory{}, got.Vars["input"])
	require.Equal(t, 3, got.Vars["count"])
	require.Equal(t, "route-a", got.Vars["label"])
	require.Nil(t, got.Vars["nothing"])
	require.Equal(t, map[string]any{"k": "v"}, got.Vars["blob"])
	require.Equal(t, cp.VarTypes, got.VarTypes)

	require.Equal(t, cp.History, got.History)
	require.Equal(t, cp.History.Messages[0].CallTools[0].ProviderMeta, got.History.Messages[0].CallTools[0].ProviderMeta)
	require.Equal(t, cp.History.Messages[0].Images, got.History.Messages[0].Images)

	assert.Equal(t, cp.ApprovalID, got.ApprovalID)
	assert.Equal(t, cp.PendingCalls, got.PendingCalls)
	assert.Equal(t, cp.Chain, got.Chain)
	assert.Equal(t, cp.TaskID, got.TaskID)
	assert.Equal(t, cp.RetryIndex, got.RetryIndex)
	assert.Equal(t, cp.Scope, got.Scope)
	assert.Equal(t, cp.EdgeCounts, got.EdgeCounts)
	assert.Equal(t, cp.TemplateVars, got.TemplateVars)
	assert.Equal(t, cp.ToolsAllowlist, got.ToolsAllowlist)
	assert.Equal(t, cp.HasToolsAllowlist, got.HasToolsAllowlist)
	assert.Equal(t, cp.ContextLength, got.ContextLength)
	assert.Equal(t, cp.SessionID, got.SessionID)
	assert.Equal(t, cp.MissionID, got.MissionID)
	assert.Equal(t, cp.RequestID, got.RequestID)
	assert.Equal(t, cp.ChainRef, got.ChainRef)
	assert.True(t, cp.CreatedAt.Equal(got.CreatedAt))

	// Vars is nulled here: it was compared per-key above because JSON numbers
	// in DataTypeJSON values legitimately decode as float64.
	cpNoVars, gotNoVars := *cp, *got
	cpNoVars.Vars, gotNoVars.Vars = nil, nil
	require.Equal(t, cpNoVars, gotNoVars)
}

// TestUnit_Checkpoint_VersionGate asserts a missing, future, or unmigratable
// version all refuse with ErrCheckpointVersion.
func TestUnit_Checkpoint_VersionGate(t *testing.T) {
	_, err := UnmarshalCheckpoint([]byte(`{"approval_id":"x"}`))
	require.ErrorIs(t, err, ErrCheckpointVersion, "a payload without schema_version must refuse")

	future := fmt.Sprintf(`{"schema_version":%d}`, CheckpointSchemaVersion+1)
	_, err = UnmarshalCheckpoint([]byte(future))
	require.ErrorIs(t, err, ErrCheckpointVersion, "a newer-than-binary payload must refuse and name the fix")
	require.Contains(t, err.Error(), "update the binary")

	_, err = UnmarshalCheckpoint([]byte(`{"schema_version":0}`))
	require.ErrorIs(t, err, ErrCheckpointVersion)

	_, err = UnmarshalCheckpoint([]byte(`not json`))
	require.Error(t, err)
}

// TestUnit_Checkpoint_MigrationHookApplies asserts migration steps apply
// sequentially, a missing step refuses, and a failing step surfaces its error.
func TestUnit_Checkpoint_MigrationHookApplies(t *testing.T) {
	const from, target = 41, 43
	for v := from; v < target; v++ {
		step := v
		checkpointMigrations[step] = func(raw []byte) ([]byte, error) {
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, err
			}
			m["schema_version"] = step + 1
			m["task_id"] = fmt.Sprintf("migrated-%d", step+1)
			return json.Marshal(m)
		}
	}
	defer func() {
		for v := from; v < target; v++ {
			delete(checkpointMigrations, v)
		}
	}()

	payload := fmt.Sprintf(`{"schema_version":%d,"approval_id":"a1"}`, from)
	raw, version, err := migrateCheckpointPayload([]byte(payload), from, target)
	require.NoError(t, err)
	require.Equal(t, target, version)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Equal(t, "a1", m["approval_id"])
	require.Equal(t, fmt.Sprintf("migrated-%d", target), m["task_id"], "both migration steps must have applied in order")

	_, _, err = migrateCheckpointPayload([]byte(payload), from, target+1)
	require.ErrorIs(t, err, ErrCheckpointVersion)

	checkpointMigrations[from] = func([]byte) ([]byte, error) { return nil, errors.New("bad step") }
	_, _, err = migrateCheckpointPayload([]byte(payload), from, from+1)
	require.ErrorContains(t, err, "bad step")
}

// TestUnit_Checkpoint_VarTypeMismatchRefuses asserts a var whose payload
// contradicts its declared type refuses to decode.
func TestUnit_Checkpoint_VarTypeMismatchRefuses(t *testing.T) {
	payload := fmt.Sprintf(`{"schema_version":%d,"vars":{"x":{"type":"int","value":"\"not an int\""}}}`, CheckpointSchemaVersion)
	_, err := UnmarshalCheckpoint([]byte(payload))
	require.Error(t, err)
	require.Contains(t, err.Error(), `var "x"`)
}

// TestUnit_Checkpoint_ApprovalVerdictContext pins the injected-verdict
// plumbing the resume path and the HITL wrapper meet on.
func TestUnit_Checkpoint_ApprovalVerdictContext(t *testing.T) {
	ctx := WithApprovalVerdicts(t.Context(), map[string]bool{"call-1": true, "call-2": false})

	approved, ok := ApprovalVerdictFromContext(ctx, "call-1")
	require.True(t, ok)
	require.True(t, approved)

	approved, ok = ApprovalVerdictFromContext(ctx, "call-2")
	require.True(t, ok)
	require.False(t, approved)

	_, ok = ApprovalVerdictFromContext(ctx, "call-3")
	require.False(t, ok)

	_, ok = ApprovalVerdictFromContext(t.Context(), "call-1")
	require.False(t, ok)

	_, ok = ApprovalVerdictFromContext(ctx, "")
	require.False(t, ok)
}

// TestUnit_Checkpoint_UnansweredToolCalls covers the pending-set derivation
// over the partial-batch transcript shape a suspension records.
func TestUnit_Checkpoint_UnansweredToolCalls(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "do it"},
		{Role: "assistant", CallTools: []ToolCall{
			{ID: "c1", Function: FunctionCall{Name: "a", Arguments: "{}"}},
			{ID: "c2", Function: FunctionCall{Name: "b", Arguments: `{"x":1}`}},
			{ID: "c3", Function: FunctionCall{Name: "c", Arguments: "{}"}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "done"},
	}
	pending := unansweredToolCalls(msgs)
	require.Equal(t, []PendingToolCall{
		{CallID: "c2", Name: "b", Arguments: `{"x":1}`},
		{CallID: "c3", Name: "c", Arguments: "{}"},
	}, pending)

	require.Nil(t, unansweredToolCalls([]Message{{Role: "user", Content: "hi"}}))
	require.Nil(t, unansweredToolCalls(nil))
}

// TestUnit_ApprovalPendingErrors pins the typed-error surface other layers
// branch on.
func TestUnit_ApprovalPendingErrors(t *testing.T) {
	pend := &ApprovalPendingError{ApprovalID: "a1", ToolName: "write_file"}
	wrapped := fmt.Errorf("task exec: %w", pend)
	var got *ApprovalPendingError
	require.True(t, errors.As(wrapped, &got))
	require.Equal(t, "a1", got.ApprovalID)

	susp := &ChainSuspendedError{ApprovalID: "a1", Scope: EventScope{Chain: "c", Task: "t", ToolCall: "a1"}}
	require.Contains(t, susp.Error(), "a1")
	require.Contains(t, susp.Error(), "resume")
}
