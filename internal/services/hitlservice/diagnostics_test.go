package hitlservice

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUnit_PolicyDiagnostics_WarnsOnUnimplementedPauseAsk pins the warning's shape for the one unenforced field.
func TestUnit_PolicyDiagnostics_WarnsOnUnimplementedPauseAsk(t *testing.T) {
	diags := PolicyDiagnostics([]byte(`{
		"default_action": "approve",
		"rules": [],
		"compute": {"maxToolCalls": 10, "onExhausted": "pause_ask"}
	}`))
	require.Len(t, diags, 1)
	require.Equal(t, "compute.onExhausted", diags[0].Field)
	require.Contains(t, diags[0].Message, "NOT IMPLEMENTED")
	require.Contains(t, diags[0].Message, "finish_stuck")
	require.Contains(t, diags[0].Message, "files no ask")
	require.Contains(t, diags[0].String(), "compute.onExhausted: ")
}

// TestUnit_PolicyDiagnostics_SilentOnEnvelopesThatDoNotUseThem pins that an
// envelope using only enforced fields produces no diagnostics.
func TestUnit_PolicyDiagnostics_SilentOnEnvelopesThatDoNotUseThem(t *testing.T) {
	for _, doc := range []string{
		`{"default_action": "approve", "rules": []}`,
		`{"default_action": "approve", "rules": [], "compute": {"maxToolCalls": 300, "maxTokens": 2000000, "onExhausted": "finish_stuck"}}`,
		`{"default_action": "approve", "rules": [], "compute": {"maxTurns": 1}}`,
		`{"default_action": "approve", "rules": [], "compute": {"modelAllowlist": ["gemini-2.5-flash"], "backendAllowlist": ["my-ollama"]}}`,
	} {
		require.Nil(t, PolicyDiagnostics([]byte(doc)), "expected silence for %s", doc)
	}
}

// TestUnit_PolicyDiagnostics_SilentOnUnparseableDocuments pins that a document that does not parse produces no diagnostics.
func TestUnit_PolicyDiagnostics_SilentOnUnparseableDocuments(t *testing.T) {
	require.Nil(t, PolicyDiagnostics([]byte(`not json at all`)))
	require.Nil(t, PolicyDiagnostics(nil))
}

// TestUnit_ComputeDiagnostics_MatchesTheAuthoringTimeWording pins that
// ComputeDiagnostics and PolicyDiagnostics agree word for word.
func TestUnit_ComputeDiagnostics_MatchesTheAuthoringTimeWording(t *testing.T) {
	require.Nil(t, ComputeDiagnostics(nil))
	require.Nil(t, ComputeDiagnostics(&ComputeBounds{MaxTurns: 5}))

	fromStruct := ComputeDiagnostics(&ComputeBounds{OnExhausted: OnExhaustedPauseAsk})
	require.Len(t, fromStruct, 1)

	raw, err := json.Marshal(Policy{
		DefaultAction: ActionApprove,
		Compute:       &ComputeBounds{OnExhausted: OnExhaustedPauseAsk},
	})
	require.NoError(t, err)
	require.Equal(t, fromStruct, PolicyDiagnostics(raw))
}

// TestUnit_PolicyDiagnostics_WarnedEnvelopeStillPassesValidation pins that a
// warned envelope still loads and vets cleanly.
func TestUnit_PolicyDiagnostics_WarnedEnvelopeStillPassesValidation(t *testing.T) {
	doc := []byte(`{
		"default_action": "approve",
		"rules": [],
		"compute": {"maxToolCalls": 10, "onExhausted": "pause_ask"}
	}`)
	require.NoError(t, VetPolicy(doc), "a warning must not fail the vet")
	require.NotEmpty(t, PolicyDiagnostics(doc))
}
