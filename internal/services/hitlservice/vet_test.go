package hitlservice

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnit_VetPolicy_AcceptsShippedShapes(t *testing.T) {
	// The shape every shipped preset uses, including the "//"-comment convention.
	good := `{
		"//compute": "add a compute block to bound total spend",
		"default_action": "approve",
		"rules": [
			{"tools": "local_fs", "tool": "read_file", "action": "allow"},
			{"tools": "*", "tool": "*", "action": "deny",
			 "when": [{"key": "path", "op": "glob", "value": "**/.ssh/**"}]},
			{"tools": "local_shell", "tool": "local_shell", "action": "approve",
			 "timeout_s": 300, "on_timeout": "deny"}
		],
		"attention": {"allowAgentAnswers": true, "maxAgentAnswers": 5},
		"compute": {"maxTurns": 1, "onExhausted": "finish_stuck"}
	}`
	require.NoError(t, VetPolicy([]byte(good)))
}

func TestUnit_VetPolicy_RejectsMalformedJSON(t *testing.T) {
	err := VetPolicy([]byte(`{"rules": [`))
	require.ErrorIs(t, err, ErrEnvelopeVet)
	require.Contains(t, err.Error(), "not a JSON object")
}

func TestUnit_VetPolicy_UnknownFields(t *testing.T) {
	cases := []struct {
		name, doc, want string
	}{
		{
			name: "top level",
			doc:  `{"default_actoin": "allow", "rules": []}`,
			want: `policy: unknown field "default_actoin"`,
		},
		{
			name: "rule level",
			doc:  `{"rules": [{"tools": "local_fs", "tool": "sed", "action": "approve", "timeout": 30}]}`,
			want: `rule 0: unknown field "timeout"`,
		},
		{
			name: "condition level",
			doc:  `{"rules": [{"tools": "local_fs", "tool": "sed", "action": "deny", "when": [{"key": "path", "operator": "glob", "value": "x"}]}]}`,
			want: `rule 0, condition 0: unknown field "operator"`,
		},
		{
			name: "compute level",
			doc:  `{"rules": [], "compute": {"maxTurn": 5}}`,
			want: `compute: unknown field "maxTurn"`,
		},
		{
			name: "attention level",
			doc:  `{"rules": [], "attention": {"maxAgentAnswer": 5}}`,
			want: `attention: unknown field "maxAgentAnswer"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := VetPolicy([]byte(tc.doc))
			require.ErrorIs(t, err, ErrEnvelopeVet)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestUnit_VetPolicy_InvalidRuleShapes(t *testing.T) {
	// Comes from the runtime's own validatePolicy, reused by vet.
	err := VetPolicy([]byte(`{"rules": [{"tools": "x", "tool": "y", "action": "permit"}]}`))
	require.ErrorIs(t, err, ErrEnvelopeVet)
	require.Contains(t, err.Error(), `unknown action "permit"`)

	err = VetPolicy([]byte(`{"rules": [{"tools": "x", "tool": "y", "action": "approve", "on_timeout": "allow"}]}`))
	require.ErrorIs(t, err, ErrEnvelopeVet)
	require.Contains(t, err.Error(), "would silently bypass approval")
}

func TestUnit_VetPolicy_ToolPatternsThatNeverMatch(t *testing.T) {
	err := VetPolicy([]byte(`{"rules": [{"tools": "local_*", "tool": "*", "action": "deny"}]}`))
	require.ErrorIs(t, err, ErrEnvelopeVet)
	require.Contains(t, err.Error(), `tools "local_*" can never match`)
	require.Contains(t, err.Error(), "compared exactly")
}

func TestUnit_VetPolicy_TimeoutValues(t *testing.T) {
	err := VetPolicy([]byte(`{"rules": [{"tools": "x", "tool": "y", "action": "approve", "timeout_s": -5}]}`))
	require.ErrorIs(t, err, ErrEnvelopeVet)
	require.Contains(t, err.Error(), "timeout_s must not be negative")

	err = VetPolicy([]byte(`{"rules": [{"tools": "x", "tool": "y", "action": "approve", "timeout_s": 999999999}]}`))
	require.ErrorIs(t, err, ErrEnvelopeVet)
	require.Contains(t, err.Error(), "out of range")

	// A timeout on a deny rule is dead config: deny never waits.
	err = VetPolicy([]byte(`{"rules": [{"tools": "x", "tool": "y", "action": "deny", "timeout_s": 30}]}`))
	require.ErrorIs(t, err, ErrEnvelopeVet)
	require.Contains(t, err.Error(), "never waits")
}

func TestUnit_VetPolicy_CollectsEverythingAtOnce(t *testing.T) {
	doc := `{
		"default_actoin": "allow",
		"rules": [
			{"tools": "local_*", "tool": "*", "action": "permit", "timeout": 5}
		]
	}`
	err := VetPolicy([]byte(doc))
	require.ErrorIs(t, err, ErrEnvelopeVet)
	for _, want := range []string{
		`unknown field "default_actoin"`,
		`unknown field "timeout"`,
		`unknown action "permit"`,
		`tools "local_*" can never match`,
	} {
		require.Contains(t, err.Error(), want)
	}
}

// TestUnit_VetPolicy_ShippedDefaultPolicyShapePasses pins that the built-in
// default policy round-trips through JSON and passes vet.
func TestUnit_VetPolicy_ShippedDefaultPolicyShapePasses(t *testing.T) {
	p := defaultPolicy()
	data, err := json.Marshal(p)
	require.NoError(t, err)
	require.NoError(t, VetPolicy(data))
}
