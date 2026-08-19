package searchtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/workspaceindex"
)

const testWorkspaceID = "ws-test"

// fakeQuerier is the whole test rig: one method, no store, no embedding model,
// no network. It also RECORDS what it was asked, which is how the argument
// matrix below asserts coercion instead of guessing at it.
type fakeQuerier struct {
	hits []workspaceindex.Hit
	err  error

	calls        int
	gotWorkspace string
	gotQuestion  string
	gotTopK      int
}

func (f *fakeQuerier) Query(_ context.Context, workspaceID, question string, topK int) ([]workspaceindex.Hit, error) {
	f.calls++
	f.gotWorkspace, f.gotQuestion, f.gotTopK = workspaceID, question, topK
	return f.hits, f.err
}

func newTools(q Querier) taskengine.ToolsRepo { return NewTools(q, testWorkspaceID) }

// exec runs the tool the way the engine does and returns the typed result.
func exec(t *testing.T, repo taskengine.ToolsRepo, args map[string]any) (*Result, taskengine.DataType, error) {
	t.Helper()
	out, dt, err := repo.Exec(context.Background(), time.Now(), args, false, &taskengine.ToolsCall{Name: ToolSearch})
	if err != nil {
		return nil, dt, err
	}
	res, ok := out.(*Result)
	if !ok {
		t.Fatalf("result is %T, want *Result", out)
	}
	return res, dt, nil
}

func hit(path string, start, end int, text string) workspaceindex.Hit {
	return workspaceindex.Hit{Path: path, StartLine: start, EndLine: end, Text: text, Score: 0.5}
}

// --- contract ---------------------------------------------------------------

func TestUnit_Tools_SupportsNamesTheProviderAndOneTool(t *testing.T) {
	got, err := newTools(&fakeQuerier{}).Supports(context.Background())
	if err != nil {
		t.Fatalf("Supports: %v", err)
	}
	want := []string{ToolsProviderName, ToolSearch}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Supports() = %v, want %v", got, want)
	}
	// These two names are HITL policy keys; pinning them forces a rename to ask the envelope question.
	// The provider name is also the name an allowlist addresses — see the gate test.
	if ToolsProviderName != "native-workspace" {
		t.Fatalf("provider name = %q, want native-workspace", ToolsProviderName)
	}
	if ToolSearch != "workspace_search" {
		t.Fatalf("tool name = %q, want workspace_search", ToolSearch)
	}
}

func TestUnit_Tools_SchemaShape(t *testing.T) {
	repo := newTools(&fakeQuerier{})
	all, err := repo.GetToolsForToolsByName(context.Background(), ToolsProviderName)
	if err != nil {
		t.Fatalf("GetToolsForToolsByName: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("%d tools, want 1", len(all))
	}
	tool := all[0]
	if tool.Type != "function" || tool.Function.Name != ToolSearch {
		t.Fatalf("tool = %+v", tool)
	}

	// The statements that cannot be re-taught by an error that never fires.
	for _, want := range []string{
		"ANY language", // it is not a Go-only tool
		"KEYWORD",      // ranking is not vector-only, so an identifier is a good query
		"exact identifier",
		"file:line-range",     // it cites locations
		"APPROXIMATELY RIGHT", // it answers from text, not a type checker
		"NOT a live",          // it is not a filesystem read
		"contenox index",      // a missing index is a runnable instruction
	} {
		if !strings.Contains(tool.Function.Description, want) {
			t.Errorf("description does not state %q:\n%s", want, tool.Function.Description)
		}
	}

	params, ok := tool.Function.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("parameters are %T, want a JSON Schema object", tool.Function.Parameters)
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("no properties: %+v", params)
	}
	for _, key := range []string{"question", "top_k"} {
		if _, ok := props[key]; !ok {
			t.Errorf("schema has no %q property", key)
		}
	}
	if len(props) != 2 {
		t.Errorf("schema declares %d properties, want exactly question and top_k", len(props))
	}
	req, ok := params["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "question" {
		t.Errorf("required = %v, want [question]", params["required"])
	}

	// Addressing one tool by name, and an unknown name.
	one, err := repo.GetToolsForToolsByName(context.Background(), ToolSearch)
	if err != nil || len(one) != 1 {
		t.Fatalf("by tool name: %v / %d", err, len(one))
	}
	if _, err := repo.GetToolsForToolsByName(context.Background(), "nope"); err == nil {
		t.Fatal("unknown tool name must be refused")
	}
}

func TestUnit_Tools_NilCallAndUnknownToolAreRefused(t *testing.T) {
	repo := newTools(&fakeQuerier{})
	if _, _, err := repo.Exec(context.Background(), time.Now(), nil, false, nil); err == nil {
		t.Fatal("a nil call must be refused")
	}
	_, _, err := repo.Exec(context.Background(), time.Now(), map[string]any{}, false, &taskengine.ToolsCall{Name: "not_a_tool"})
	if err == nil {
		t.Fatal("an unknown tool must be refused")
	}
	if !strings.Contains(err.Error(), severityRecoverable) {
		t.Errorf("unknown-tool error carries no severity marker: %v", err)
	}
	if !strings.Contains(err.Error(), ToolSearch) {
		t.Errorf("unknown-tool error does not name what this toolset provides: %v", err)
	}
}

// --- argument matrix --------------------------------------------------------

func TestUnit_Tools_TopKCoercionAndClamping(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  int
	}{
		{"absent", nil, topKDefault},
		{"int", 3, 3},
		{"float", float64(4), 4},
		{"string int (small models emit these)", "6", 6},
		{"json.Number", json.Number("7"), 7},
		{"zero takes the default", 0, topKDefault},
		{"negative takes the default", -12, topKDefault},
		{"above the ceiling clamps", 10000, topKMax},
		{"1e30 saturates then clamps", 1e30, topKMax},
		{"NaN reads as no value", nan(), topKDefault},
		{"-1e30 saturates then defaults", -1e30, topKDefault},
		{"unparseable string reads as no value", "many", topKDefault},
		{"empty string reads as no value", "", topKDefault},
		{"bool is not a number", true, topKDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQuerier{hits: []workspaceindex.Hit{hit("a.md", 1, 2, "x")}}
			args := map[string]any{"question": "where is retry backoff"}
			if tc.value != nil {
				args["top_k"] = tc.value
			}
			if _, _, err := exec(t, newTools(q), args); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if q.gotTopK != tc.want {
				t.Errorf("top_k %v -> %d, want %d", tc.value, q.gotTopK, tc.want)
			}
		})
	}
}

func nan() float64 { var z float64; return z / z * 0 } //nolint:staticcheck // deliberate NaN

func TestUnit_Tools_QuestionCoercionAndWorkspaceScoping(t *testing.T) {
	q := &fakeQuerier{hits: []workspaceindex.Hit{hit("a.md", 1, 2, "x")}}
	if _, _, err := exec(t, newTools(q), map[string]any{"question": "  spaced out  "}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if q.gotQuestion != "spaced out" {
		t.Errorf("question = %q, want it trimmed", q.gotQuestion)
	}
	// The workspace is a property of the process, never of the call.
	if q.gotWorkspace != testWorkspaceID {
		t.Errorf("workspace = %q, want %q", q.gotWorkspace, testWorkspaceID)
	}
}

func TestUnit_Tools_EmptyQuestionIsRefusedWithoutQuerying(t *testing.T) {
	for _, arg := range []any{"", "   ", nil, 12.5} {
		q := &fakeQuerier{}
		args := map[string]any{"top_k": 3}
		if arg != nil {
			args["question"] = arg
		}
		_, _, err := exec(t, newTools(q), args)
		if arg == 12.5 {
			// A number coerces to a non-empty question, answered by the index, not this layer.
			if err != nil {
				t.Fatalf("numeric question: %v", err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("question %#v must be refused", arg)
		}
		if !strings.Contains(err.Error(), severityRecoverable) {
			t.Errorf("no severity marker: %v", err)
		}
		if q.calls != 0 {
			t.Error("a refused question must never reach the index (an embed call costs money)")
		}
	}
}

func TestUnit_Tools_UnknownArgumentsAreRefused(t *testing.T) {
	q := &fakeQuerier{}
	_, _, err := exec(t, newTools(q), map[string]any{"question": "x", "topK": 3, "path": "/etc"})
	if err == nil {
		t.Fatal("unknown argument names must be refused")
	}
	for _, want := range []string{"topK", "path", "question", "top_k", severityRecoverable} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
	if q.calls != 0 {
		t.Error("a refused call must never reach the index")
	}
}

func TestUnit_Tools_HostileArgumentNamesAreClampedAndSanitised(t *testing.T) {
	long := strings.Repeat("Ω", 5_000)
	_, _, err := exec(t, newTools(&fakeQuerier{}), map[string]any{
		"question":              "x",
		long:                    1,
		"nul\x00bidi\u202ename": 1,
	})
	if err == nil {
		t.Fatal("unknown argument names must be refused")
	}
	msg := err.Error()
	if len([]rune(msg)) > 2_000 {
		t.Errorf("error is %d runes — an echoed argument name is not a length channel", len([]rune(msg)))
	}
	if strings.ContainsRune(msg, '\x00') || strings.ContainsRune(msg, '\u202e') {
		t.Error("non-printable runes must be replaced, not embedded")
	}
}

func TestUnit_Tools_HostileQuestionIsClampedBeforeEmbedding(t *testing.T) {
	q := &fakeQuerier{}
	long := strings.Repeat("a", 50_000)
	res, _, err := exec(t, newTools(q), map[string]any{"question": long})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := len([]rune(q.gotQuestion)); got != maxQuestionRunes {
		t.Errorf("embedded question is %d runes, want it clamped to %d", got, maxQuestionRunes)
	}
	if got := len([]rune(res.Question)); got > maxEchoRunes+1 {
		t.Errorf("echoed question is %d runes, want it clamped", got)
	}
	if !strings.Contains(res.Note, "truncated") {
		t.Errorf("a clamped question must say so: %q", res.Note)
	}
}

func TestUnit_Tools_ArgsMayArriveOnTheCallInsteadOfTheInput(t *testing.T) {
	q := &fakeQuerier{hits: []workspaceindex.Hit{hit("a.md", 1, 2, "x")}}
	out, _, err := newTools(q).Exec(context.Background(), time.Now(), nil, false, &taskengine.ToolsCall{
		ToolName: ToolSearch,
		// Declarative args are strings on the wire, hence the lenient coercion above.
		Args: map[string]string{"question": "declarative", "top_k": "2"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if _, ok := out.(*Result); !ok {
		t.Fatalf("result is %T", out)
	}
	if q.gotQuestion != "declarative" || q.gotTopK != 2 {
		t.Errorf("declarative args not read: %q / %d", q.gotQuestion, q.gotTopK)
	}
}

// --- results ----------------------------------------------------------------

func TestUnit_Tools_HitsAreCitationsWithJSONDataType(t *testing.T) {
	q := &fakeQuerier{hits: []workspaceindex.Hit{
		{Path: "docs/retry.md", StartLine: 10, EndLine: 24, Text: "backoff doubles", Score: 0.81234567},
	}}
	res, dt, err := exec(t, newTools(q), map[string]any{"question": "backoff"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if dt != taskengine.DataTypeJSON {
		t.Errorf("data type = %v, want JSON", dt)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("%d hits, want 1", len(res.Hits))
	}
	got := res.Hits[0]
	if got.Citation != "docs/retry.md:10-24" {
		t.Errorf("citation = %q", got.Citation)
	}
	if got.Score != 0.8123 {
		t.Errorf("score = %v, want it rounded to 4 places", got.Score)
	}
	if res.Found != 1 || res.Shown != 1 || res.Note != "" {
		t.Errorf("an untruncated, current result needs no note: %+v", res)
	}
	// It must survive the engine's own serialisation unchanged.
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"citation":"docs/retry.md:10-24"`, `"start_line":10`, `"end_line":24`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("payload missing %s: %s", want, raw)
		}
	}
}

func TestUnit_Tools_EmptyResultSaysSoHonestly(t *testing.T) {
	res, _, err := exec(t, newTools(&fakeQuerier{hits: nil}), map[string]any{"question": "nothing matches this"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Hits == nil {
		t.Error("hits must be an empty slice, never null")
	}
	if len(res.Hits) != 0 || res.Found != 0 {
		t.Fatalf("%+v", res)
	}
	for _, want := range []string{"No indexed chunk matched", "contenox index", "snapshot"} {
		if !strings.Contains(res.Note, want) {
			t.Errorf("empty-result note does not say %q: %q", want, res.Note)
		}
	}
}

func TestUnit_Tools_NoIndexIsAnInstructionNotAnError(t *testing.T) {
	q := &fakeQuerier{err: fmt.Errorf("%w (workspace ws-test)", workspaceindex.ErrNoIndex)}
	res, dt, err := exec(t, newTools(q), map[string]any{"question": "anything"})
	if err != nil {
		t.Fatalf("a missing index must NOT be an error: %v", err)
	}
	if dt != taskengine.DataTypeJSON {
		t.Errorf("data type = %v", dt)
	}
	if len(res.Hits) != 0 {
		t.Errorf("%d hits, want none", len(res.Hits))
	}
	if !strings.Contains(res.Note, "contenox index") {
		t.Errorf("note must name the command that fixes it: %q", res.Note)
	}
	if !strings.Contains(res.Note, "not a failure") {
		t.Errorf("note must say outright that this is not a fault of the tool: %q", res.Note)
	}
}

// An index that exists but is empty is a different claim from "nothing
// matched", and ErrIndexEmpty wraps ErrNoIndex, so the wrong note is one
// mis-ordered errors.Is away.
func TestUnit_Tools_EmptyIndexIsNotTheSameAsNoIndex(t *testing.T) {
	q := &fakeQuerier{err: fmt.Errorf("%w (workspace ws-test, index cfg-1)", workspaceindex.ErrIndexEmpty)}
	res, dt, err := exec(t, newTools(q), map[string]any{"question": "anything"})
	if err != nil {
		t.Fatalf("an empty index must NOT be an error: %v", err)
	}
	if dt != taskengine.DataTypeJSON {
		t.Errorf("data type = %v", dt)
	}
	if len(res.Hits) != 0 {
		t.Errorf("%d hits, want none", len(res.Hits))
	}
	if !strings.Contains(res.Note, "holds no chunks") {
		t.Errorf("note must say the index is empty, not that nothing exists: %q", res.Note)
	}
	if !strings.Contains(res.Note, "NOT evidence") {
		t.Errorf("note must refuse to be read as an absence proof: %q", res.Note)
	}
	if !strings.Contains(res.Note, "contenox index") {
		t.Errorf("note must name the command that fixes it: %q", res.Note)
	}
}

// The no-index note is also where a human learns that indexing does not
// require an embedding model — the keyword leg answers without one.
func TestUnit_Tools_NoIndexNoteSaysIndexingNeedsNoEmbeddingModel(t *testing.T) {
	q := &fakeQuerier{err: fmt.Errorf("%w (workspace ws-test)", workspaceindex.ErrNoIndex)}
	res, _, err := exec(t, newTools(q), map[string]any{"question": "anything"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for _, want := range []string{"does not require an embedding model", "keyword-only"} {
		if !strings.Contains(res.Note, want) {
			t.Errorf("no-index note does not state %q: %q", want, res.Note)
		}
	}
}

func TestUnit_Tools_OtherIndexFailuresStayErrorsWithSeverity(t *testing.T) {
	q := &fakeQuerier{err: errors.New("store: disk I/O error")}
	if _, _, err := exec(t, newTools(q), map[string]any{"question": "x"}); err == nil {
		t.Fatal("a real index failure must be an error")
	} else if !strings.Contains(err.Error(), severityRecoverable) {
		t.Errorf("no severity marker: %v", err)
	}

	// An error that already carries a marker is not double-tagged.
	q = &fakeQuerier{err: errors.New("store: broken " + severityRecoverable)}
	_, _, err := exec(t, newTools(q), map[string]any{"question": "x"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Count(err.Error(), severityRecoverable) != 1 {
		t.Errorf("severity marker was doubled: %v", err)
	}
}

func TestUnit_Tools_StaleHitsAreFlaggedAndCounted(t *testing.T) {
	q := &fakeQuerier{hits: []workspaceindex.Hit{
		{Path: "a.md", StartLine: 1, EndLine: 2, Text: "current", Score: 0.9},
		{Path: "b.md", StartLine: 3, EndLine: 4, Text: "moved underneath", Score: 0.8, Stale: true},
	}}
	res, _, err := exec(t, newTools(q), map[string]any{"question": "x"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Hits[0].Stale || !res.Hits[1].Stale {
		t.Fatalf("staleness not carried through: %+v", res.Hits)
	}
	if res.Stale != 1 {
		t.Errorf("stale count = %d, want 1", res.Stale)
	}
	// A stale hit is still returned, never as if it were current.
	if res.Hits[1].Text == "" {
		t.Error("a stale hit must still carry its text")
	}
	if !strings.Contains(res.Note, "stale") {
		t.Errorf("note must name the staleness: %q", res.Note)
	}
}

func TestUnit_Tools_PayloadIsTokenCappedAndSaysWhatItWithheld(t *testing.T) {
	// Twenty hits of two thousand runes each: far past the budget.
	var hits []workspaceindex.Hit
	for i := range 20 {
		hits = append(hits, workspaceindex.Hit{
			Path: fmt.Sprintf("f%02d.md", i), StartLine: 1, EndLine: 100,
			Text: strings.Repeat("x", 2_000), Score: 1 - float64(i)/100,
		})
	}
	res, _, err := exec(t, newTools(&fakeQuerier{hits: hits}), map[string]any{"question": "x", "top_k": 20})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Found != 20 {
		t.Errorf("found = %d, want the pre-cap count of 20", res.Found)
	}
	if res.Shown >= res.Found {
		t.Fatalf("shown = %d of %d — the budget did not bite", res.Shown, res.Found)
	}
	total := 0
	for _, h := range res.Hits {
		total += len([]rune(h.Text))
	}
	if total > resultRuneBudget+len(res.Hits)*80 { // + the per-hit "… (+N …)" markers
		t.Errorf("payload is %d runes, past the ~%d-rune budget", total, resultRuneBudget)
	}
	// Ranking order survives the cap: the hits kept are the top ones.
	if res.Hits[0].Path != "f00.md" {
		t.Errorf("first hit = %s, want the highest-ranked", res.Hits[0].Path)
	}
	if !res.Hits[0].Truncated || !strings.Contains(res.Hits[0].Text, "not shown") {
		t.Error("a clipped chunk must be marked truncated AND say so in its text")
	}
	if !strings.Contains(res.Note, "TRUNCATED") || !strings.Contains(res.Note, "withheld") {
		t.Errorf("note must carry an explicit truncation marker: %q", res.Note)
	}
	if !strings.Contains(res.Note, fmt.Sprintf("%d of %d", res.Found-res.Shown, res.Found)) {
		t.Errorf("note must say how many were withheld: %q", res.Note)
	}
}

func TestUnit_Tools_ShortHitsAreNotClipped(t *testing.T) {
	res, _, err := exec(t, newTools(&fakeQuerier{hits: []workspaceindex.Hit{
		hit("a.md", 1, 3, "three short lines\nof text\nthat fit"),
	}}), map[string]any{"question": "x"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Hits[0].Truncated || res.Hits[0].Text != "three short lines\nof text\nthat fit" {
		t.Errorf("a chunk inside the budget must come back verbatim: %+v", res.Hits[0])
	}
	if res.Note != "" {
		t.Errorf("nothing was withheld, so there is nothing to note: %q", res.Note)
	}
}

// TestUnit_Tools_PublishedSchemaMatchesToolDescriptor pins the declared
// OpenAPI contract and its agreement with what actually reaches the provider:
// the request schema and the descriptor's parameters are rendered from one
// property table (searchProperties), so they must agree property for property,
// and the response schema must describe the payload Exec really returns.
func TestUnit_Tools_PublishedSchemaMatchesToolDescriptor(t *testing.T) {
	repo := newTools(&fakeQuerier{})
	ctx := context.Background()

	docs, err := repo.GetSchemasForSupportedTools(ctx)
	if err != nil {
		t.Fatalf("GetSchemasForSupportedTools: %v", err)
	}
	doc, ok := docs[ToolsProviderName]
	if !ok {
		t.Fatalf("no document published under %q, got %v", ToolsProviderName, docs)
	}
	if doc.OpenAPI != "3.1.0" {
		t.Errorf("openapi version = %q", doc.OpenAPI)
	}
	if doc.Info == nil || doc.Info.Title == "" || doc.Info.Description == "" || doc.Info.Version == "" {
		t.Fatalf("document is not described: %+v", doc.Info)
	}
	if doc.Components == nil {
		t.Fatal("document declares no components")
	}
	if err := doc.Validate(ctx); err != nil {
		t.Errorf("the published document is not a valid OpenAPI document: %v", err)
	}
	if len(doc.Components.Schemas) != 2 {
		t.Errorf("%d component schemas, want a request and a response for the one tool", len(doc.Components.Schemas))
	}

	// The component names are part of the contract: a rename is a breaking change.
	req := doc.Components.Schemas["WorkspaceSearchRequest"]
	if req == nil || req.Value == nil {
		t.Fatal("no WorkspaceSearchRequest schema is published")
	}
	resp := doc.Components.Schemas["WorkspaceSearchResponse"]
	if resp == nil || resp.Value == nil {
		t.Fatal("no WorkspaceSearchResponse schema is published")
	}

	declared, err := repo.GetToolsForToolsByName(ctx, ToolSearch)
	if err != nil || len(declared) != 1 {
		t.Fatalf("GetToolsForToolsByName: %v / %d", err, len(declared))
	}
	params := declared[0].Function.Parameters.(map[string]any)
	props := params["properties"].(map[string]any)
	if len(props) != len(req.Value.Properties) {
		t.Errorf("descriptor declares %d properties, the published schema %d", len(props), len(req.Value.Properties))
	}
	for name, published := range req.Value.Properties {
		declaredProp, ok := props[name].(map[string]any)
		if !ok {
			t.Errorf("%s is published but the descriptor does not declare it", name)
			continue
		}
		if published.Value.Type == nil || !published.Value.Type.Is(declaredProp["type"].(string)) {
			t.Errorf("%s: published type %v, descriptor type %v", name, published.Value.Type, declaredProp["type"])
		}
		if published.Value.Description == "" {
			t.Errorf("%s: published without a description", name)
		}
		if published.Value.Description != declaredProp["description"] {
			t.Errorf("%s: descriptor and published schema disagree on the description", name)
		}
		// Nothing here has a closed value set, in either place.
		if len(published.Value.Enum) != 0 {
			t.Errorf("%s: published an enum the descriptor does not declare", name)
		}
	}
	wantRequired, _ := params["required"].([]string)
	if strings.Join(wantRequired, ",") != strings.Join(req.Value.Required, ",") {
		t.Errorf("required = %v, descriptor requires %v", req.Value.Required, wantRequired)
	}

	for name, published := range resp.Value.Properties {
		if published.Value.Description == "" {
			t.Errorf("response: %s is published without a description", name)
		}
		if published.Value.Type == nil {
			t.Errorf("response: %s is published without a type", name)
		}
	}
	hits := resp.Value.Properties["hits"]
	if hits == nil || hits.Value.Items == nil {
		t.Fatal("response: hits is published without an item schema")
	}
	for name, published := range hits.Value.Items.Value.Properties {
		if published.Value.Description == "" || published.Value.Type == nil {
			t.Errorf("hit: %s is published without a type or a description", name)
		}
	}

	// Declared against what a call actually produces, field for field.
	res, _, err := exec(t, newTools(&fakeQuerier{hits: []workspaceindex.Hit{
		{Path: "a.md", StartLine: 1, EndLine: 3, Text: "some text", Score: 0.5, Stale: true},
	}}), map[string]any{"question": "where is retry backoff configured"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	for name := range got {
		if _, ok := resp.Value.Properties[name]; !ok {
			t.Errorf("the result carries %s but the published schema does not declare it", name)
		}
	}
	for _, name := range resp.Value.Required {
		if _, ok := got[name]; !ok {
			t.Errorf("the published schema requires %s but the result omits it", name)
		}
	}
	gotHit := got["hits"].([]any)[0].(map[string]any)
	for name := range gotHit {
		if _, ok := hits.Value.Items.Value.Properties[name]; !ok {
			t.Errorf("a hit carries %s but the published schema does not declare it", name)
		}
	}
	for _, name := range hits.Value.Items.Value.Required {
		if _, ok := gotHit[name]; !ok {
			t.Errorf("the published hit schema requires %s but the hit omits it", name)
		}
	}
}
