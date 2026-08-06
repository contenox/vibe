package oracletools_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/contenox/contenox/internal/services/oracletools"
	"github.com/stretchr/testify/require"
)

// recordingAnswerer records deliveries; err scripts the next outcomes.
type recordingAnswerer struct {
	calls []string
	texts []string
	errs  []error
}

func (a *recordingAnswerer) Answer(_ context.Context, askID, text string) error {
	a.calls = append(a.calls, askID)
	a.texts = append(a.texts, text)
	if len(a.errs) > 0 {
		err := a.errs[0]
		a.errs = a.errs[1:]
		return err
	}
	return nil
}

const askID = "ask-42"
const inputJSON = `{"askId":"ask-42","summary":"proceed?"}`

func boundProvider(t *testing.T) (taskengine.ToolsRepo, *recordingAnswerer, *oracletools.AskBinding, context.Context) {
	t.Helper()
	answerer := &recordingAnswerer{}
	p := oracletools.New(answerer)
	binding := oracletools.NewAskBinding(askID, inputJSON)
	return p, answerer, binding, oracletools.WithBinding(context.Background(), binding)
}

// submitResult is submit_verdict's declared response, decoded.
type submitResult struct {
	accepted bool
	outcome  string
	message  string
}

func submit(t *testing.T, p taskengine.ToolsRepo, ctx context.Context, args map[string]any) (submitResult, error) {
	t.Helper()
	out, dt, err := p.Exec(ctx, time.Now(), args, false, &taskengine.ToolsCall{
		Name: oracletools.ToolsProviderName, ToolName: oracletools.ToolNameSubmitVerdict,
	})
	if err != nil {
		return submitResult{}, err
	}
	require.Equal(t, taskengine.DataTypeJSON, dt, "submit_verdict returns its declared structured response")
	m, ok := out.(map[string]any)
	require.True(t, ok, "submit_verdict results are objects")
	require.Len(t, m, 3, "exactly the declared response properties")
	accepted, ok := m["accepted"].(bool)
	require.True(t, ok, "accepted is a boolean")
	outcome, ok := m["outcome"].(string)
	require.True(t, ok, "outcome is a string")
	message, ok := m["message"].(string)
	require.True(t, ok, "message is a string")
	return submitResult{accepted: accepted, outcome: outcome, message: message}, nil
}

func gate(t *testing.T, p taskengine.ToolsRepo, ctx context.Context) map[string]any {
	t.Helper()
	out, dt, err := p.Exec(ctx, time.Now(), nil, false, &taskengine.ToolsCall{
		Name: oracletools.ToolsProviderName, ToolName: oracletools.ToolNameVerdictState,
	})
	require.NoError(t, err)
	require.Equal(t, taskengine.DataTypeJSON, dt)
	m, ok := out.(map[string]any)
	require.True(t, ok)
	return m
}

// TestUnit_OracleTools_UnboundExecutionSeesNothing pins the mission-tools
// idiom: no binding, no tool listed, and Exec refuses.
func TestUnit_OracleTools_UnboundExecutionSeesNothing(t *testing.T) {
	p := oracletools.New(&recordingAnswerer{})
	tools, err := p.GetToolsForToolsByName(context.Background(), oracletools.ToolsProviderName)
	require.NoError(t, err)
	require.Empty(t, tools, "off a bound execution the provider lists no tools at all")

	_, _, err = p.Exec(context.Background(), time.Now(), map[string]any{"verdict": "wait", "askId": askID}, false,
		&taskengine.ToolsCall{Name: oracletools.ToolsProviderName, ToolName: oracletools.ToolNameSubmitVerdict})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no ask is bound to this execution")
}

// TestUnit_OracleTools_BoundExecutionListsSubmitVerdictOnly pins the exposed
// surface: exactly submit_verdict; the gate tool is never advertised.
func TestUnit_OracleTools_BoundExecutionListsSubmitVerdictOnly(t *testing.T) {
	p, _, _, ctx := boundProvider(t)
	tools, err := p.GetToolsForToolsByName(ctx, oracletools.ToolsProviderName)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	require.Equal(t, oracletools.ToolNameSubmitVerdict, tools[0].Function.Name)
}

// TestUnit_OracleTools_ValidationMatrix pins every corrective result: a
// malformed call returns a RESULT naming exactly what was wrong (never an
// error), the contract stays open, and the answerer is never consulted.
func TestUnit_OracleTools_ValidationMatrix(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want []string
	}{
		{
			name: "unknown verdict names the exact schema",
			args: map[string]any{"verdict": "approve", "askId": askID},
			want: []string{`invalid verdict "approve"`, `{"verdict":"wait"|"answer"`, "submit the verdict via submit_verdict again"},
		},
		{
			name: "missing verdict names the exact schema",
			args: map[string]any{"askId": askID},
			want: []string{`invalid verdict ""`},
		},
		{
			name: "wrong askId names the expected source field",
			args: map[string]any{"verdict": "answer", "answer": "yes", "askId": "ask-imagined"},
			want: []string{`invalid askId "ask-imagined"`, "askId must be the askId field of the INPUT event", `"ask-42"`},
		},
		{
			name: "answer verdict without answer text",
			args: map[string]any{"verdict": "answer", "askId": askID},
			want: []string{`verdict "answer" requires a non-empty "answer"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, answerer, binding, ctx := boundProvider(t)
			out, err := submit(t, p, ctx, tc.args)
			require.NoError(t, err, "a corrective outcome is a result, not an error")
			require.False(t, out.accepted, "a rejected call is declared rejected, not left to prose")
			require.Empty(t, out.outcome, "nothing was recorded")
			for _, want := range tc.want {
				require.Contains(t, out.message, want)
			}
			require.Empty(t, answerer.calls, "an invalid call never reaches the answerer")
			require.Equal(t, oracletools.OutcomeNone, binding.Outcome(), "the contract stays open for the retry")
		})
	}
}

// TestUnit_OracleTools_WaitSettlesWithoutDelivery pins the WAIT verdict:
// acknowledged, settled, nothing delivered.
func TestUnit_OracleTools_WaitSettlesWithoutDelivery(t *testing.T) {
	p, answerer, binding, ctx := boundProvider(t)
	out, err := submit(t, p, ctx, map[string]any{"verdict": "wait", "askId": askID})
	require.NoError(t, err)
	require.True(t, out.accepted)
	require.Equal(t, "wait", out.outcome)
	require.Contains(t, out.message, "WAIT")
	require.Contains(t, out.message, "do not call submit_verdict again")
	require.Empty(t, answerer.calls)
	require.Equal(t, oracletools.OutcomeWait, binding.Outcome())
}

// TestUnit_OracleTools_AnswerDeliversAndSettles pins the ANSWER verdict and
// the settled idempotence of a repeat call.
func TestUnit_OracleTools_AnswerDeliversAndSettles(t *testing.T) {
	p, answerer, binding, ctx := boundProvider(t)
	out, err := submit(t, p, ctx, map[string]any{"verdict": "answer", "answer": "Yes, proceed.", "askId": askID})
	require.NoError(t, err)
	require.True(t, out.accepted)
	require.Equal(t, "answered", out.outcome)
	require.Contains(t, out.message, "ANSWER delivered")
	require.Equal(t, []string{askID}, answerer.calls)
	require.Equal(t, []string{"Yes, proceed."}, answerer.texts)
	require.Equal(t, oracletools.OutcomeAnswered, binding.Outcome())
	require.Equal(t, "Yes, proceed.", binding.Answer())

	again, err := submit(t, p, ctx, map[string]any{"verdict": "answer", "answer": "again", "askId": askID})
	require.NoError(t, err)
	require.False(t, again.accepted)
	require.Contains(t, again.message, "verdict already recorded")
	require.Len(t, answerer.calls, 1, "a settled contract never delivers twice")
}

// TestUnit_OracleTools_PolicyDenialIsPlainAndOpen pins the founder contract:
// a refusal returns a plain denied-per-policy result — no counts, no reasons,
// no coaching — the contract stays open, a retry yields the same denial, and
// a subsequent wait settles cleanly.
func TestUnit_OracleTools_PolicyDenialIsPlainAndOpen(t *testing.T) {
	p, answerer, binding, ctx := boundProvider(t)
	answerer.errs = []error{
		&oracletools.AnswerRefusedError{Reason: "envelope forbids"},
		&oracletools.AnswerRefusedError{Reason: "envelope forbids"},
	}

	out, err := submit(t, p, ctx, map[string]any{"verdict": "answer", "answer": "yes", "askId": askID})
	require.NoError(t, err)
	require.False(t, out.accepted)
	require.Empty(t, out.outcome)
	require.Equal(t, "answer denied per policy for ask ask-42.", out.message,
		"a denial is one plain statement: no bound counts, no remedy, no alternative")
	require.Equal(t, oracletools.OutcomeNone, binding.Outcome(), "a denial never settles the contract")

	retry, err := submit(t, p, ctx, map[string]any{"verdict": "answer", "answer": "yes", "askId": askID})
	require.NoError(t, err)
	require.Equal(t, out, retry, "retrying yields the same denial")

	wait, err := submit(t, p, ctx, map[string]any{"verdict": "wait", "askId": askID})
	require.NoError(t, err)
	require.True(t, wait.accepted)
	require.Contains(t, wait.message, "WAIT")
	require.Equal(t, oracletools.OutcomeWait, binding.Outcome())
}

// TestUnit_OracleTools_TransientErrorSurfacesAndRetries pins the transient
// path: the error surfaces (execute_tool_calls renders it as the tool
// result), the contract stays open, and a later valid call succeeds.
func TestUnit_OracleTools_TransientErrorSurfacesAndRetries(t *testing.T) {
	p, answerer, binding, ctx := boundProvider(t)
	answerer.errs = []error{errors.New("db locked")}

	_, err := submit(t, p, ctx, map[string]any{"verdict": "answer", "answer": "yes", "askId": askID})
	require.Error(t, err)
	require.Contains(t, err.Error(), "db locked")
	require.Equal(t, oracletools.OutcomeNone, binding.Outcome())

	out, err := submit(t, p, ctx, map[string]any{"verdict": "answer", "answer": "yes", "askId": askID})
	require.NoError(t, err)
	require.True(t, out.accepted)
	require.Contains(t, out.message, "ANSWER delivered")
	require.Equal(t, oracletools.OutcomeAnswered, binding.Outcome())
}

// TestUnit_OracleTools_VerdictStateGate pins the deterministic gate: open
// renders the machine-register correction carrying the original input;
// settled renders the terminal state.
func TestUnit_OracleTools_VerdictStateGate(t *testing.T) {
	p, _, _, ctx := boundProvider(t)

	open := gate(t, p, ctx)
	require.Equal(t, "open", open["state"])
	text, _ := open["text"].(string)
	require.Contains(t, text, "output rejected: submit the verdict via submit_verdict")
	require.Contains(t, text, inputJSON, "the correction re-renders the original INPUT")

	_, err := submit(t, p, ctx, map[string]any{"verdict": "wait", "askId": askID})
	require.NoError(t, err)
	settled := gate(t, p, ctx)
	require.Equal(t, "settled", settled["state"])
}

// TestUnit_OracleTools_DeterministicArgsShape pins the secondary call shape
// (ToolsCall.Args), the missiontools idiom's deterministic path.
func TestUnit_OracleTools_DeterministicArgsShape(t *testing.T) {
	p, answerer, binding, ctx := boundProvider(t)
	out, _, err := p.Exec(ctx, time.Now(), nil, false, &taskengine.ToolsCall{
		Name: oracletools.ToolsProviderName, ToolName: oracletools.ToolNameSubmitVerdict,
		Args: map[string]string{"verdict": "answer", "answer": "yes", "askId": askID},
	})
	require.NoError(t, err)
	require.Contains(t, out.(map[string]any)["message"].(string), "ANSWER delivered")
	require.True(t, out.(map[string]any)["accepted"].(bool))
	require.Equal(t, []string{askID}, answerer.calls)
	require.Equal(t, oracletools.OutcomeAnswered, binding.Outcome())
}

// TestUnit_OracleTools_PublishedSchemaMatchesToolDescriptor pins the declared
// contract and its consistency with what actually reaches the provider: the
// OpenAPI request schema and the tool descriptor's parameters must agree
// property for property — types, descriptions, enums, and the required set.
func TestUnit_OracleTools_PublishedSchemaMatchesToolDescriptor(t *testing.T) {
	p, _, _, ctx := boundProvider(t)

	docs, err := p.GetSchemasForSupportedTools(ctx)
	require.NoError(t, err)
	doc, ok := docs[oracletools.ToolsProviderName]
	require.True(t, ok, "the toolset publishes its contract under its provider name")
	require.Equal(t, "3.1.0", doc.OpenAPI)
	require.NotNil(t, doc.Info)
	require.NotEmpty(t, doc.Info.Title)
	require.NotEmpty(t, doc.Info.Description)
	require.NotEmpty(t, doc.Info.Version)
	require.NotNil(t, doc.Components)

	req := doc.Components.Schemas["SubmitVerdictRequest"]
	require.NotNil(t, req, "the request contract is declared")
	require.ElementsMatch(t, []string{"verdict", "askId"}, req.Value.Required)
	require.Len(t, req.Value.Properties, 3)
	require.ElementsMatch(t, []any{"wait", "answer"}, req.Value.Properties["verdict"].Value.Enum,
		"the allowed verdicts are declared as an enum, not prose")
	for name, prop := range req.Value.Properties {
		require.Truef(t, prop.Value.Type.Is("string"), "%s is typed", name)
		require.NotEmptyf(t, prop.Value.Description, "%s is described", name)
	}

	// Every response the tools actually return is declared.
	resp := doc.Components.Schemas["SubmitVerdictResponse"]
	require.NotNil(t, resp)
	require.ElementsMatch(t, []string{"accepted", "outcome", "message"}, resp.Value.Required)
	for name, prop := range resp.Value.Properties {
		require.NotEmptyf(t, prop.Value.Description, "%s is described", name)
	}
	require.True(t, resp.Value.Properties["accepted"].Value.Type.Is("boolean"))
	require.ElementsMatch(t, []any{"", "wait", "answered"}, resp.Value.Properties["outcome"].Value.Enum)

	gateResp := doc.Components.Schemas["VerdictStateResponse"]
	require.NotNil(t, gateResp, "the deterministic gate's response is declared too")
	require.ElementsMatch(t, []any{"settled", "open"}, gateResp.Value.Properties["state"].Value.Enum)

	// The descriptor is what reaches the provider: it must not drift from the
	// declaration above.
	tools, err := p.GetToolsForToolsByName(ctx, oracletools.ToolsProviderName)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	params, ok := tools[0].Function.Parameters.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "object", params["type"])
	require.ElementsMatch(t, req.Value.Required, params["required"].([]string))

	props, ok := params["properties"].(map[string]any)
	require.True(t, ok)
	require.Len(t, props, len(req.Value.Properties))
	for name, declared := range req.Value.Properties {
		prop, ok := props[name].(map[string]any)
		require.Truef(t, ok, "descriptor declares %s", name)
		require.Equal(t, "string", prop["type"], name)
		require.Equal(t, declared.Value.Description, prop["description"],
			"%s: descriptor and published schema must carry the same description", name)
		if len(declared.Value.Enum) == 0 {
			require.NotContainsf(t, prop, "enum", "%s declares no enum in either place", name)
			continue
		}
		enum, ok := prop["enum"].([]string)
		require.Truef(t, ok, "%s: descriptor enum", name)
		asAny := make([]any, 0, len(enum))
		for _, v := range enum {
			asAny = append(asAny, v)
		}
		require.ElementsMatch(t, declared.Value.Enum, asAny,
			"%s: descriptor and published schema must declare the same values", name)
	}
}
