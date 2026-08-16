package contenoxcli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeEnvelope(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}

// TestUnit_MissionEnvelopes_ListsSearchPathFirstWins asserts discovery mirrors the policy loader: the workspace dir shadows home, and only hitl-policy-*.json counts.
func TestUnit_MissionEnvelopes_ListsSearchPathFirstWins(t *testing.T) {
	t.Parallel()
	workspace, home := t.TempDir(), t.TempDir()
	writeEnvelope(t, workspace, "hitl-policy-default.json", `{"default_action":"deny","rules":[]}`)
	writeEnvelope(t, home, "hitl-policy-default.json", `{"default_action":"allow","rules":[]}`)
	writeEnvelope(t, home, "hitl-policy-strict.json", `{"default_action":"deny","rules":[]}`)
	writeEnvelope(t, home, "chain-agent-beam.json", `{"tasks":[]}`)

	got := missionEnvelopes{dirs: []string{workspace, home}}.ListEnvelopes()
	require.Len(t, got, 2, "one entry per name, chains excluded: %+v", got)
	require.Equal(t, "hitl-policy-default.json", got[0].Name)
	require.Equal(t, filepath.Join(workspace, "hitl-policy-default.json"), got[0].Path,
		"the workspace copy shadows home, exactly as the loader resolves it")
	require.Contains(t, got[0].Summary, "denied")
	require.Equal(t, "hitl-policy-strict.json", got[1].Name)
}

// TestUnit_MissionEnvelopes_LookupResolvesAndRefusesTraversal asserts a name resolves along the search path and a path-shaped name never leaves the config dirs.
func TestUnit_MissionEnvelopes_LookupResolvesAndRefusesTraversal(t *testing.T) {
	t.Parallel()
	workspace, home := t.TempDir(), t.TempDir()
	writeEnvelope(t, home, "hitl-policy-strict.json", `{"default_action":"deny","rules":[]}`)
	src := missionEnvelopes{dirs: []string{workspace, home}}

	env, ok := src.LookupEnvelope("hitl-policy-strict.json")
	require.True(t, ok)
	require.Equal(t, filepath.Join(home, "hitl-policy-strict.json"), env.Path)

	for _, name := range []string{"", "  ", "hitl-policy-nope.json", "../hitl-policy-strict.json", "/etc/passwd"} {
		_, ok := src.LookupEnvelope(name)
		require.Falsef(t, ok, "%q must not resolve", name)
	}
}

// TestUnit_EnvelopeSummary_StatesCharacter asserts the one-line sketch names
// the fall-through, the tool-call ceiling, and who may answer a unit's
// questions.
func TestUnit_EnvelopeSummary_StatesCharacter(t *testing.T) {
	t.Parallel()

	t.Run("agent answers permitted", func(t *testing.T) {
		t.Parallel()
		got := envelopeSummary([]byte(`{"default_action":"approve","compute":{"maxToolCalls":300},"attention":{"allowAgentAnswers":true,"maxAgentAnswers":3}}`))
		require.Contains(t, got, "stop for approval")
		require.Contains(t, got, "≤300 tool calls (declared, not enforced)")
		require.Contains(t, got, "an agent may answer 3 questions")
	})

	t.Run("agent answers forbidden", func(t *testing.T) {
		t.Parallel()
		got := envelopeSummary([]byte(`{"default_action":"deny","compute":{"maxToolCalls":150},"attention":{"allowAgentAnswers":false}}`))
		require.Contains(t, got, "denied")
		require.Contains(t, got, "questions always wait for a human")
	})

	t.Run("omitted cap falls to the enforced default", func(t *testing.T) {
		t.Parallel()
		require.Contains(t,
			envelopeSummary([]byte(`{"attention":{"allowAgentAnswers":true}}`)),
			"an agent may answer 3 questions")
	})

	t.Run("unparseable file gets no summary rather than a wrong one", func(t *testing.T) {
		t.Parallel()
		require.Empty(t, envelopeSummary([]byte(`not json`)))
	})
}

// TestUnit_MissionEnvelopes_ShippedPresetsAllSummarize asserts every preset this build ships renders a character line, so the listing is never half blank.
func TestUnit_MissionEnvelopes_ShippedPresetsAllSummarize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, p := range HITLPolicyPresets {
		writeEnvelope(t, dir, p.Name, p.Content)
	}
	got := missionEnvelopes{dirs: []string{dir}}.ListEnvelopes()
	require.Len(t, got, len(HITLPolicyPresets))
	for _, e := range got {
		require.NotEmptyf(t, e.Summary, "preset %s has no summary", e.Name)
	}
}
