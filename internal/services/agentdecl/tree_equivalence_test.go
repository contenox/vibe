package agentdecl_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/agentdecl"
	"github.com/stretchr/testify/require"
)

// shape is what "equivalent" means here: the routing graph, not the prose.
type shape struct {
	handler string
	tools   []string
	edges   map[string]string // operator+when -> goto
}

func shapeOf(tasks []taskengine.TaskDefinition) map[string]shape {
	out := map[string]shape{}
	for _, t := range tasks {
		s := shape{handler: string(t.Handler), edges: map[string]string{}}
		if t.ExecuteConfig != nil {
			s.tools = append([]string(nil), t.ExecuteConfig.Tools...)
			sort.Strings(s.tools)
		}
		for _, b := range t.Transition.Branches {
			key := string(b.Operator) + "/" + b.When
			// Round counts are POLICY, not shape: agents.toml owns them, and a
			// converted agent takes its bounds from there rather than from the
			// number the hand-written chain froze in. Compared separately below.
			if b.Operator == taskengine.OpEdgeTraversedAtLeast {
				key = string(b.Operator)
			}
			s.edges[key] = b.Goto
		}
		if t.Transition.OnFailure != "" {
			s.edges["on_failure"] = t.Transition.OnFailure
		}
		out[t.ID] = s
	}
	return out
}

// The shipped ACP chain, expressed as a directory, emits the same routing
// graph. This is the claim the convention rests on, checked against the real
// artifact rather than a fixture.
func TestUnit_Tree_MatchesTheShippedACPChain(t *testing.T) {
	for _, name := range []string{"acp", "beam", "contenox"} {
		t.Run(name, func(t *testing.T) { requireMatchesShipped(t, name) })
	}
}

// requireMatchesShipped diffs one converted tree against the chain it replaces.
func requireMatchesShipped(t *testing.T, name string) {
	cfg := mustShipped(t)
	tree, err := agentdecl.LoadTree(filepath.Join("preseed", "agents", name), cfg)
	require.NoError(t, err)

	got, err := agentdecl.EmitTree(tree, cfg)
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join("..", "..", "surfaces", "contenoxcli", "chain-agent-"+name+".json"))
	require.NoError(t, err)
	var want taskengine.TaskChainDefinition
	require.NoError(t, json.Unmarshal(raw, &want))

	gotShape, wantShape := shapeOf(got.Tasks), shapeOf(want.Tasks)
	require.Equal(t, len(wantShape), len(gotShape),
		"same number of tasks: shipped=%d emitted=%d", len(wantShape), len(gotShape))

	// The shipped ids are hand-chosen; the emitted ones are path-derived. Map
	// one onto the other so the comparison is about the GRAPH, not the naming.
	// The shipped general branch is spelled per chain; everything else matches.
	generalChat := "acp_chat"
	if name == "contenox" {
		generalChat = "contenox_chat"
	}
	// The emitted ids are prefixed by the agent's declared NAME, which is the
	// name the shipped chain already had (chain-acp, chain-contenox).
	id := "chain-" + name
	alias := map[string]string{
		id + "-route":                  "classify_request",
		id + "-coding-agent":           "coding_chat",
		id + "-coding-tools":           "coding_tools",
		id + "-coding-recovery":        "coding_recovery",
		id + "-coding-recovery-tools":  "coding_recovery_tools",
		id + "-review-agent":           "review_chat",
		id + "-review-tools":           "review_tools",
		id + "-general-agent":          generalChat,
		id + "-general-tools":          "run_tools",
		id + "-general-recovery":       "recovery_chat",
		id + "-general-recovery-tools": "recovery_tools",
		id + "-summarise":              "summarise_failure",
	}
	// Branch LABELS are intentionally renamed: the shipped chain spells them
	// coding_change/review_change while a directory spells itself coding/review.
	// That rename is the feature — the label is the directory name, so the
	// prompt and the branch cannot drift — so the comparison aliases them and
	// checks the graph they describe.
	label := map[string]string{
		"equals/coding":  "equals/coding_change",
		"equals/review":  "equals/review_change",
		"equals/general": "equals/general",
	}
	aliasEdge := func(key string) string {
		if k, ok := label[key]; ok {
			return k
		}
		return key
	}

	for gotID, gotS := range gotShape {
		wantID, ok := alias[gotID]
		require.True(t, ok, "emitted task %q has no counterpart in the shipped chain", gotID)
		wantS, ok := wantShape[wantID]
		require.True(t, ok, "shipped chain has no %q", wantID)
		require.Equal(t, wantS.handler, gotS.handler, "handler for %s", gotID)

		// Compare by the shipped chain's own edge keys, aliasing the renamed labels.
		aliased := map[string]string{}
		for key, goTo := range gotS.edges {
			aliased[aliasEdge(key)] = goTo
		}
		for key, wantGoto := range wantS.edges {
			gotGoto, has := aliased[key]
			require.True(t, has, "%s is missing edge %s (shipped goes to %s)", gotID, key, wantGoto)
			mapped := alias[gotGoto]
			if mapped == "" {
				mapped = gotGoto // terminals like "end" are not renamed
			}
			require.Equal(t, mapped, wantGoto,
				"%s edge %s: emitted -> %s, shipped -> %s", gotID, key, gotGoto, wantGoto)
		}
	}
}

// The conversion is shape-preserving but NOT bound-preserving: the shipped
// chains froze recovery at 8 rounds, while agents.toml's default is 10. This
// pins the delta so it is a decision on the record rather than a surprise —
// set [agents.acp.chain] recovery_rounds = 8 to keep the old number.
func TestUnit_Tree_RecoveryBoundComesFromConfigNotTheOldChain(t *testing.T) {
	cfg := mustShipped(t)
	require.Equal(t, 60, cfg.Chain.MainRounds, "main rounds already match the shipped chains")
	require.Equal(t, 10, cfg.Chain.RecoveryRounds,
		"shipped chains hardcode 8; a converted agent takes 10 from agents.toml unless overridden")
}
