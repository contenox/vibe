package searchtool

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/workspaceindex"
)

// withPolicy attaches a chain's [tools_policies.native-workspace] block the way
// the engine does: keyed by the toolset name, as strings.
func withPolicy(args map[string]string) context.Context {
	return taskengine.WithToolsArgs(context.Background(), ToolsProviderName, args)
}

// execCtx runs the tool on a caller-supplied context, which is the only way the
// policy reaches it.
func execCtx(t *testing.T, ctx context.Context, repo taskengine.ToolsRepo, args map[string]any) *Result {
	t.Helper()
	out, _, err := repo.Exec(ctx, time.Now(), args, false, &taskengine.ToolsCall{Name: ToolsProviderName, ToolName: ToolSearch})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	res, ok := out.(*Result)
	if !ok {
		t.Fatalf("result is %T, want *Result", out)
	}
	return res
}

// TestUnit_Policy_TopKCeilingComesFromTheToolsPolicy pins the args plumbing:
// the ceiling a call clamps to is the chain's, not the compiled default.
func TestUnit_Policy_TopKCeilingComesFromTheToolsPolicy(t *testing.T) {
	cases := []struct {
		name     string
		policy   map[string]string
		asked    int
		wantTopK int
	}{
		{"no policy keeps the default ceiling", nil, 50, topKMax},
		{"a tighter ceiling binds", map[string]string{policyMaxTopK: "3"}, 50, 3},
		{"a tighter ceiling also caps the default", map[string]string{policyMaxTopK: "2"}, 0, 2},
		{"a request under the ceiling is untouched", map[string]string{policyMaxTopK: "9"}, 4, 4},
		{"a raised ceiling admits more", map[string]string{policyMaxTopK: "40"}, 50, 40},
		{"past the hard bound clamps to it", map[string]string{policyMaxTopK: "100000"}, 999, topKCeilingMax},
		{"zero cannot silence the tool", map[string]string{policyMaxTopK: "0"}, 5, topKCeilingMin},
		{"negative cannot silence the tool", map[string]string{policyMaxTopK: "-4"}, 5, topKCeilingMin},
		{"garbage falls back to the default", map[string]string{policyMaxTopK: "lots"}, 50, topKMax},
		{"an empty value falls back to the default", map[string]string{policyMaxTopK: ""}, 50, topKMax},
		{"whitespace is tolerated", map[string]string{policyMaxTopK: " 6 "}, 50, 6},
		{"an unrelated key is ignored", map[string]string{"_allowed_dir": "."}, 50, topKMax},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQuerier{hits: []workspaceindex.Hit{hit("a.md", 1, 2, "x")}}
			args := map[string]any{"question": "where is retry backoff"}
			if tc.asked > 0 {
				args["top_k"] = tc.asked
			}
			execCtx(t, withPolicy(tc.policy), newTools(q), args)
			if q.gotTopK != tc.wantTopK {
				t.Errorf("top_k reaching the index = %d, want %d", q.gotTopK, tc.wantTopK)
			}
		})
	}
}

// TestUnit_Policy_ResultBudgetComesFromTheToolsPolicy pins the other half: how
// much context one result may cost is the chain's decision.
func TestUnit_Policy_ResultBudgetComesFromTheToolsPolicy(t *testing.T) {
	var hits []workspaceindex.Hit
	for i := range 20 {
		hits = append(hits, workspaceindex.Hit{
			Path: fmt.Sprintf("f%02d.md", i), StartLine: 1, EndLine: 100,
			Text: strings.Repeat("x", 2_000), Score: 1 - float64(i)/100,
		})
	}

	tight := execCtx(t, withPolicy(map[string]string{policyMaxResultTokens: "200"}), newTools(&fakeQuerier{hits: hits}),
		map[string]any{"question": "x", "top_k": 20})
	wide := execCtx(t, withPolicy(map[string]string{policyMaxResultTokens: "20000"}), newTools(&fakeQuerier{hits: hits}),
		map[string]any{"question": "x", "top_k": 20})

	if tight.Shown >= wide.Shown {
		t.Fatalf("a tightened budget showed %d hits, a widened one %d", tight.Shown, wide.Shown)
	}
	if tight.Found != 20 || wide.Found != 20 {
		t.Errorf("found must stay the pre-cap count: %d / %d", tight.Found, wide.Found)
	}
	total := 0
	for _, h := range tight.Hits {
		total += len([]rune(h.Text))
	}
	if total > 200*runesPerToken+len(tight.Hits)*80 {
		t.Errorf("tightened payload is %d runes, past its %d-token budget", total, 200)
	}
	// The note quotes the budget that actually bit, not the compiled default.
	if !strings.Contains(tight.Note, "~200-token") {
		t.Errorf("truncation note does not name the policy's budget: %q", tight.Note)
	}
}

// A policy attached to another toolset's key must not reach this one; the args
// context is per-toolset and a leak would let one declaration retune another.
func TestUnit_Policy_IsScopedToThisToolsetsName(t *testing.T) {
	ctx := taskengine.WithToolsArgs(context.Background(), "local_fs", map[string]string{policyMaxTopK: "1"})
	q := &fakeQuerier{hits: []workspaceindex.Hit{hit("a.md", 1, 2, "x")}}
	execCtx(t, ctx, newTools(q), map[string]any{"question": "x", "top_k": 12})
	if q.gotTopK != 12 {
		t.Errorf("top_k = %d, want 12 — another toolset's policy was applied", q.gotTopK)
	}
}

// The descriptor the model reads is rendered from the same limits the call
// enforces, so a policy cannot advertise a ceiling that is not real.
func TestUnit_Policy_DescriptorAdvertisesTheEffectiveCeiling(t *testing.T) {
	repo := newTools(&fakeQuerier{})
	ctx := withPolicy(map[string]string{policyMaxTopK: "3", policyMaxResultTokens: "400"})

	declared, err := repo.GetToolsForToolsByName(ctx, ToolsProviderName)
	if err != nil {
		t.Fatalf("GetToolsForToolsByName: %v", err)
	}
	props := declared[0].Function.Parameters.(map[string]any)["properties"].(map[string]any)
	desc := props["top_k"].(map[string]any)["description"].(string)
	for _, want := range []string{"ceiling 3", "~400 tokens"} {
		if !strings.Contains(desc, want) {
			t.Errorf("descriptor does not state %q under the active policy:\n%s", want, desc)
		}
	}
	// A ceiling below the default default also moves the advertised default.
	if !strings.Contains(desc, "default 3") {
		t.Errorf("descriptor advertises a default above its own ceiling:\n%s", desc)
	}

	// The published contract is rendered from the same table.
	docs, err := repo.GetSchemasForSupportedTools(ctx)
	if err != nil {
		t.Fatalf("GetSchemasForSupportedTools: %v", err)
	}
	doc, ok := docs[ToolsProviderName]
	if !ok {
		t.Fatalf("no document published under %q, got %v", ToolsProviderName, docs)
	}
	published := doc.Components.Schemas["WorkspaceSearchRequest"].Value.Properties["top_k"].Value.Description
	if published != desc {
		t.Errorf("published schema and descriptor disagree under policy:\n%s\n%s", published, desc)
	}
}

// The policy tightens the payload; it is not a second approval gate. A refused
// call is refused by the wrapper above, so nothing here may turn a policy value
// into a denial.
func TestUnit_Policy_NeverRefusesACall(t *testing.T) {
	for _, args := range []map[string]string{
		{policyMaxTopK: "0"},
		{policyMaxResultTokens: "0"},
		{policyMaxTopK: "-1", policyMaxResultTokens: "-1"},
	} {
		q := &fakeQuerier{hits: []workspaceindex.Hit{hit("a.md", 1, 2, "short enough")}}
		res := execCtx(t, withPolicy(args), newTools(q), map[string]any{"question": "x"})
		if q.calls != 1 {
			t.Errorf("policy %v stopped the call reaching the index", args)
		}
		if res.Found != 1 {
			t.Errorf("policy %v suppressed the ranking itself: %+v", args, res)
		}
	}
}
