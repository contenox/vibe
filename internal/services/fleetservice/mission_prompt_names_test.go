package fleetservice

import (
	"context"
	"strings"
	"testing"

	"github.com/contenox/beam/internal/kernel/taskengine"
	"github.com/contenox/beam/internal/services/missionservice"
	"github.com/contenox/beam/internal/services/missiontools"
	"github.com/stretchr/testify/require"
)

// TestUnit_MissionPrompts_NameToolsAsTheModelSeesThem is the regression for a
// silent, expensive drift: the unattended preamble and nudge named the mission
// tools BARE (`mission_report`), while taskengine offers every tool qualified by
// its provider ("mission.mission_report") and resolves calls by that same
// qualified name. A dispatched unit was therefore ordered — twice — to call a
// function absent from its own tool list. It answered in prose, was nudged,
// returned an empty turn, and the runtime filed a blocker against a unit that had
// done the work and had no reachable way to report it.
//
// The prompts are now derived from the tool package's constants, so this test
// guards the remaining gap that derivation cannot close on its own: that the
// QUALIFICATION rule the prompts assume is the one taskengine actually applies.
func TestUnit_MissionPrompts_NameToolsAsTheModelSeesThem(t *testing.T) {
	// The names taskengine will put in the model's tool list, derived the way it
	// derives them (taskenv.go: toolsName + "." + tool.Function.Name).
	tools, err := missiontools.New(schemaOnlyStore{}, nil).GetToolsForToolsByName(
		missiontools.WithMissionID(context.Background(), "m-1"),
		missiontools.ToolsProviderName,
	)
	require.NoError(t, err)
	require.NotEmpty(t, tools, "the mission tools must be listed for a session on a mission")

	offered := make(map[string]bool, len(tools))
	for _, tool := range tools {
		offered[missiontools.ToolsProviderName+"."+tool.Function.Name] = true
	}

	for _, name := range []string{toolAskAttention, toolReport, toolFinish} {
		require.True(t, offered[name],
			"the prompts tell a unit to call %q, which is not among the tools the model is offered: %v", name, keys(offered))
	}

	for _, prompt := range []struct {
		what string
		text string
	}{{"preamble", missionPreamble}, {"nudge", missionNudge}} {
		for _, name := range []string{toolAskAttention, toolReport, toolFinish} {
			require.Contains(t, prompt.text, name, "the %s must name %q the way the model sees it", prompt.what, name)
		}
		// And must not name the bare form, which is what broke: a bare mention
		// reads to the model as a different, non-existent function.
		for _, bare := range []string{
			missiontools.ToolNameAskAttention,
			missiontools.ToolNameReport,
			missiontools.ToolNameFinish,
		} {
			require.False(t, mentionsBare(prompt.text, bare),
				"the %s names the bare tool %q; the model's list only has %q.%s",
				prompt.what, bare, missiontools.ToolsProviderName, bare)
		}
	}
}

// mentionsBare reports whether text names tool WITHOUT its provider prefix —
// i.e. an occurrence not immediately preceded by "mission.".
func mentionsBare(text, tool string) bool {
	prefix := missiontools.ToolsProviderName + "."
	for i := 0; ; {
		idx := strings.Index(text[i:], tool)
		if idx < 0 {
			return false
		}
		at := i + idx
		if at < len(prefix) || text[at-len(prefix):at] != prefix {
			return true
		}
		i = at + len(tool)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// schemaOnlyStore satisfies missiontools.MissionStore for a test that only reads
// tool SCHEMAS — no method is ever called, so each is a bare stub rather than a
// fake with behaviour to keep in sync.
type schemaOnlyStore struct{}

func (schemaOnlyStore) AddReport(context.Context, string, *missionservice.Report) error {
	return nil
}

func (schemaOnlyStore) Heartbeat(context.Context, string, string) (*missionservice.Mission, error) {
	return nil, nil
}

func (schemaOnlyStore) SetPlan(context.Context, string, []missionservice.PlanEntry, string) (*missionservice.Mission, error) {
	return nil, nil
}

func (schemaOnlyStore) Finish(context.Context, string, missionservice.Status, string) (*missionservice.Mission, error) {
	return nil, nil
}

var _ taskengine.Tool // the tool shape these names are derived from
