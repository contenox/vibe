package hitlservice

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// The one unenforced field must produce its warning, and the warning must say
// three things: that it is not enforced, what it actually does instead, and what
// to rely on. An operator reading only this line must not be left believing the
// field works.
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
	// The rendered line leads with the field so it is greppable in a JSON file.
	require.Contains(t, diags[0].String(), "compute.onExhausted: ")
}

// Silence is the contract for everything else. An envelope that does not use an
// unenforced field must produce no diagnostics at all — warnings that fire on
// healthy files train an operator to ignore them.
func TestUnit_PolicyDiagnostics_SilentOnEnvelopesThatDoNotUseThem(t *testing.T) {
	for _, doc := range []string{
		// No compute block at all.
		`{"default_action": "approve", "rules": []}`,
		// A compute block using only bounds that are enforced.
		`{"default_action": "approve", "rules": [], "compute": {"maxToolCalls": 300, "maxTokens": 2000000, "onExhausted": "finish_stuck"}}`,
		// onExhausted left to its default.
		`{"default_action": "approve", "rules": [], "compute": {"maxTurns": 1}}`,
		// The allowlists are enforced, so declaring them warns about nothing.
		`{"default_action": "approve", "rules": [], "compute": {"modelAllowlist": ["gemini-2.5-flash"], "backendAllowlist": ["my-ollama"]}}`,
	} {
		require.Nil(t, PolicyDiagnostics([]byte(doc)), "expected silence for %s", doc)
	}
}

// A document that does not parse has one problem — its parse error, which the
// validator already reports. Guessing at the intent of malformed JSON would warn
// about fields the operator never wrote.
func TestUnit_PolicyDiagnostics_SilentOnUnparseableDocuments(t *testing.T) {
	require.Nil(t, PolicyDiagnostics([]byte(`not json at all`)))
	require.Nil(t, PolicyDiagnostics(nil))
}

// ComputeDiagnostics is the runtime's entry point (the envelope is already
// loaded), and must agree with the authoring-time one word for word — the whole
// point of one source for the wording is that vet and the mission record cannot
// drift.
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

// A warned envelope is still a VALID envelope: a diagnostic is a true statement
// about a working file, never a defect. It must load and vet cleanly.
func TestUnit_PolicyDiagnostics_WarnedEnvelopeStillPassesValidation(t *testing.T) {
	doc := []byte(`{
		"default_action": "approve",
		"rules": [],
		"compute": {"maxToolCalls": 10, "onExhausted": "pause_ask"}
	}`)
	require.NoError(t, VetPolicy(doc), "a warning must not fail the vet")
	require.NotEmpty(t, PolicyDiagnostics(doc))
}
