// Package oracletools is the oracle attention driver's tool grant: exactly
// one model-facing tool, submit_verdict, bound to exactly one durable ask for
// exactly one chain execution. The binding rides the request context
// (WithBinding), set once by the driver before it runs the oracle chain — the
// mission-tools idiom: off a bound execution the provider lists no tools at
// all, so nothing else ever sees submit_verdict. The provider is registered
// only in the driver's host engine, never in the global CLI toolset.
//
// This package's responsibility ends at handing a valid ANSWER to the
// Answerer seam; bounds enforcement and durable delivery live behind it
// (hitlservice.EnforceAgentAnswerBounds + AnswerAsAgentNamed, composed by the
// driver).
package oracletools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine"
	"github.com/getkin/kin-openapi/openapi3"
)

// ToolsProviderName is the tools-provider key the driver registers this
// package under; the model sees the tool as "oracle.submit_verdict".
const ToolsProviderName = "oracle"

// verdictAnswer is the "answer" verdict value; the "wait" value is
// OutcomeWait, which the verdict and the outcome share verbatim.
const verdictAnswer = "answer"

// The verdict_state gate's two states.
const (
	verdictStateSettled = "settled"
	verdictStateOpen    = "open"
)

const (
	// ToolNameSubmitVerdict is the one model-facing tool: submit the WAIT or
	// ANSWER verdict for the bound ask.
	ToolNameSubmitVerdict = "submit_verdict"
	// ToolNameVerdictState is the deterministic chain-step gate (a `tools`
	// task, never advertised to the model): it reports whether the contract
	// is settled and, when open, renders the machine-register correction the
	// loop feeds back on a chat-text reply.
	ToolNameVerdictState = "verdict_state"
)

// Outcome is a bound contract's terminal state.
type Outcome string

const (
	// OutcomeNone: no valid verdict landed (yet) — WAIT-equivalent if the
	// chain ends here.
	OutcomeNone Outcome = ""
	// OutcomeWait: a valid {"verdict":"wait"} was recorded.
	OutcomeWait Outcome = "wait"
	// OutcomeAnswered: a valid ANSWER was delivered to the ask.
	OutcomeAnswered Outcome = "answered"
)

// AskBinding binds one oracle-chain execution to one durable ask: the askId
// every submit_verdict call must echo, the input payload corrections
// re-render, and the contract's settled state.
type AskBinding struct {
	askID string
	input string

	mu      sync.Mutex
	outcome Outcome
	answer  string
}

// NewAskBinding builds the binding for askID; input is the ask payload JSON
// the chain received (re-rendered verbatim into corrections).
func NewAskBinding(askID, input string) *AskBinding {
	return &AskBinding{askID: askID, input: input}
}

// AskID returns the bound ask's id.
func (b *AskBinding) AskID() string { return b.askID }

// Outcome returns the contract's terminal state, OutcomeNone while open.
func (b *AskBinding) Outcome() Outcome {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.outcome
}

// Answer returns the delivered answer text (OutcomeAnswered only).
func (b *AskBinding) Answer() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.answer
}

func (b *AskBinding) settled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.outcome != OutcomeNone
}

func (b *AskBinding) settle(o Outcome, answer string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.outcome = o
	b.answer = answer
}

// bindingCtxKey is the unexported context key WithBinding stores under.
type bindingCtxKey struct{}

// WithBinding binds b to ctx as this execution's contract. The driver calls
// it once per oracle-chain run; a nil b returns ctx unchanged.
func WithBinding(ctx context.Context, b *AskBinding) context.Context {
	if b == nil {
		return ctx
	}
	return context.WithValue(ctx, bindingCtxKey{}, b)
}

// BindingFromContext returns the binding WithBinding set, or nil when the
// execution is not an oracle-chain run.
func BindingFromContext(ctx context.Context) *AskBinding {
	b, _ := ctx.Value(bindingCtxKey{}).(*AskBinding)
	return b
}

// Answerer delivers a valid ANSWER verdict to the bound ask. A returned
// *AnswerRefusedError is a policy denial (the envelope holding, or the ask
// already resolved): the tool reports it as a plain denied-per-policy result
// and the contract stays open — a retry yields the same denial, self-limiting
// within the chain's budgets. Any other error is transient plumbing, surfaced
// to the model as the tool result.
type Answerer interface {
	Answer(ctx context.Context, askID, text string) error
}

// AnswerRefusedError is a policy denial — see Answerer. This package discards
// Reason (the tool result is the plain denial), so the Answerer must state it
// on the operator's trace at the refusal point or the denial is invisible.
type AnswerRefusedError struct{ Reason string }

func (e *AnswerRefusedError) Error() string { return e.Reason }

type provider struct {
	answerer Answerer
}

// New returns the oracle tools provider. answerer is required — a verdict
// tool with no delivery seam is a wiring defect, so New panics rather than
// degrading (missiontools.New's stance).
func New(answerer Answerer) taskengine.ToolsRepo {
	if answerer == nil {
		panic("oracletools: answerer is required")
	}
	return &provider{answerer: answerer}
}

// Supports reports the one provider name; which tools it then exposes is
// gated on the context binding in GetToolsForToolsByName.
func (p *provider) Supports(context.Context) ([]string, error) {
	return []string{ToolsProviderName}, nil
}

// GetSchemasForSupportedTools publishes the toolset's OpenAPI 3.1 contract:
// the submit_verdict request/response pair and the deterministic gate's
// response. The request schema is built from the same property table the tool
// descriptor renders (verdictProperties), so the declared contract and what
// the model actually receives cannot drift.
func (p *provider) GetSchemasForSupportedTools(context.Context) (map[string]*openapi3.T, error) {
	schema := &openapi3.T{
		OpenAPI: "3.1.0",
		Info: &openapi3.Info{
			Title:       "Oracle Attention Tools",
			Description: "Submit the verdict for the one durable attention ask bound to this chain execution. wait leaves the question to a human; answer delivers a reply, still bounded by the mission envelope's attention bounds.",
			Version:     "1.0.0",
		},
		Paths: openapi3.NewPaths(),
		Components: &openapi3.Components{
			Schemas: map[string]*openapi3.SchemaRef{
				"SubmitVerdictRequest": {
					Value: &openapi3.Schema{
						Type:       &openapi3.Types{openapi3.TypeObject},
						Properties: verdictRequestSchemaProperties(),
						Required:   verdictRequiredProperties(),
					},
				},
				"SubmitVerdictResponse": {
					Value: &openapi3.Schema{
						Type: &openapi3.Types{openapi3.TypeObject},
						Properties: map[string]*openapi3.SchemaRef{
							"accepted": {Value: &openapi3.Schema{
								Type:        &openapi3.Types{openapi3.TypeBoolean},
								Description: "True when this call settled the contract. False for every corrective outcome (malformed shape, wrong askId, missing answer, a denied answer, a contract already settled) — read message and submit a corrected call.",
							}},
							"outcome": {Value: &openapi3.Schema{
								Type:        &openapi3.Types{openapi3.TypeString},
								Enum:        []any{string(OutcomeNone), string(OutcomeWait), string(OutcomeAnswered)},
								Description: `The contract's state after this call: "wait" (recorded, nothing executed), "answered" (the reply was delivered and the asking unit continues), or "" (still open — nothing was recorded).`,
							}},
							"message": {Value: &openapi3.Schema{
								Type:        &openapi3.Types{openapi3.TypeString},
								Description: "One line stating what happened: the acknowledgment on a settled contract, or exactly what was wrong on a corrective outcome.",
							}},
						},
						Required: []string{"accepted", "outcome", "message"},
					},
				},
				"VerdictStateResponse": {
					Value: &openapi3.Schema{
						Type: &openapi3.Types{openapi3.TypeObject},
						Properties: map[string]*openapi3.SchemaRef{
							"state": {Value: &openapi3.Schema{
								Type:        &openapi3.Types{openapi3.TypeString},
								Enum:        []any{verdictStateSettled, verdictStateOpen},
								Description: `"settled" when a verdict was recorded (the chain ends), "open" when none was.`,
							}},
							"text": {Value: &openapi3.Schema{
								Type:        &openapi3.Types{openapi3.TypeString},
								Description: "The correction to feed back to the model while the contract is open, carrying the original input; empty when settled.",
							}},
						},
						Required: []string{"state", "text"},
					},
				},
			},
		},
	}
	return map[string]*openapi3.T{ToolsProviderName: schema}, nil
}

// verdictProperty is one submit_verdict argument, declared once and rendered
// into both the OpenAPI components and the model-facing tool descriptor.
type verdictProperty struct {
	name        string
	description string
	// enum, when set, is the closed value set — declared, never left to prose.
	enum     []string
	required bool
}

// verdictProperties is the single source of truth for submit_verdict's
// arguments: every property is a string, so neither renderer carries a type
// switch.
func verdictProperties() []verdictProperty {
	return []verdictProperty{
		{
			name:        "verdict",
			description: `"wait": a human must answer. "answer": the question is ROUTINE and the answer property carries the reply.`,
			enum:        []string{string(OutcomeWait), verdictAnswer},
			required:    true,
		},
		{
			name:        "answer",
			description: "Required when verdict is answer: one short plain sentence, on one line, that the asking unit can act on. Omitted or empty for a wait verdict.",
		},
		{
			name:        "askId",
			description: "The askId field of the INPUT event, echoed exactly. A value that does not match the bound ask is refused.",
			required:    true,
		},
	}
}

// verdictRequestSchemaProperties renders the property table as OpenAPI schema refs.
func verdictRequestSchemaProperties() map[string]*openapi3.SchemaRef {
	out := make(map[string]*openapi3.SchemaRef, 3)
	for _, p := range verdictProperties() {
		s := &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Description: p.description}
		for _, v := range p.enum {
			s.Enum = append(s.Enum, v)
		}
		out[p.name] = &openapi3.SchemaRef{Value: s}
	}
	return out
}

// verdictRequiredProperties renders the table's required set, in table order.
func verdictRequiredProperties() []string {
	var out []string
	for _, p := range verdictProperties() {
		if p.required {
			out = append(out, p.name)
		}
	}
	return out
}

// verdictToolParameters renders the same table as the descriptor's JSON Schema
// parameters — what actually reaches the provider.
func verdictToolParameters() map[string]any {
	props := make(map[string]any, 3)
	for _, p := range verdictProperties() {
		prop := map[string]any{"type": "string", "description": p.description}
		if len(p.enum) > 0 {
			prop["enum"] = append([]string(nil), p.enum...)
		}
		props[p.name] = prop
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   verdictRequiredProperties(),
	}
}

// GetToolsForToolsByName lists submit_verdict only when ctx carries an ask
// binding; off a bound execution it returns an empty slice, so the tool is
// absent from every other run's tool list rather than present-and-refused.
func (p *provider) GetToolsForToolsByName(ctx context.Context, name string) ([]taskengine.Tool, error) {
	if name != ToolsProviderName {
		return nil, fmt.Errorf("unknown tools: %s", name)
	}
	if BindingFromContext(ctx) == nil {
		return []taskengine.Tool{}, nil
	}
	return []taskengine.Tool{submitVerdictToolSchema()}, nil
}

// Exec runs one oracle-tool call. It refuses off a bound execution — the
// backstop behind the empty listing above.
func (p *provider) Exec(ctx context.Context, _ time.Time, input any, _ bool, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if call == nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("oracletools: missing tools call")
	}
	binding := BindingFromContext(ctx)
	if binding == nil {
		return nil, taskengine.DataTypeAny, fmt.Errorf("oracletools: no ask is bound to this execution; %s exists only inside the attention driver's oracle-chain run", call.ToolName)
	}
	switch call.ToolName {
	case ToolNameSubmitVerdict:
		return p.execSubmitVerdict(ctx, binding, input, call)
	case ToolNameVerdictState:
		return execVerdictState(binding), taskengine.DataTypeJSON, nil
	default:
		return nil, taskengine.DataTypeAny, fmt.Errorf("oracletools: unknown tool %q (want %s or %s)", call.ToolName, ToolNameSubmitVerdict, ToolNameVerdictState)
	}
}

// verdictResult renders submit_verdict's declared response
// (SubmitVerdictResponse): accepted says whether the contract was settled,
// outcome its state after the call, message the one line naming what happened.
func verdictResult(accepted bool, outcome Outcome, message string) map[string]any {
	return map[string]any{"accepted": accepted, "outcome": string(outcome), "message": message}
}

// corrective is a rejected call: nothing recorded, the contract still open,
// message naming exactly what to fix.
func corrective(format string, args ...any) (any, taskengine.DataType, error) {
	return verdictResult(false, OutcomeNone, fmt.Sprintf(format, args...)), taskengine.DataTypeJSON, nil
}

// verdictSchemaLine is the corrective schema statement a malformed call gets
// back verbatim.
const verdictSchemaLine = `the schema is {"verdict":"wait"|"answer", "answer": string (required when verdict is "answer"), "askId": string}. Correct the call and submit the verdict via submit_verdict again.`

// execSubmitVerdict validates one submit_verdict call against the binding
// and, on a valid verdict, settles the contract. Every invalid call returns a
// corrective RESULT (never an error) naming exactly what was wrong, so the
// loop hands it back to the model for a bounded retry.
func (p *provider) execSubmitVerdict(ctx context.Context, b *AskBinding, input any, call *taskengine.ToolsCall) (any, taskengine.DataType, error) {
	if b.settled() {
		return corrective("verdict already recorded for ask %s: the contract is settled. Do not call submit_verdict again.", b.askID)
	}
	verdict := strings.ToLower(strings.TrimSpace(argString(input, call, "verdict")))
	answer := strings.TrimSpace(argString(input, call, "answer"))
	askID := strings.TrimSpace(argString(input, call, "askId"))

	if verdict != string(OutcomeWait) && verdict != verdictAnswer {
		return corrective("invalid verdict %q: %s", verdict, verdictSchemaLine)
	}
	if askID != b.askID {
		return corrective("invalid askId %q: askId must be the askId field of the INPUT event, which is %q. Correct the call and submit the verdict via submit_verdict again.", askID, b.askID)
	}
	if verdict == verdictAnswer && answer == "" {
		return corrective(`invalid call: verdict "answer" requires a non-empty "answer" — the one short plain sentence the unit acts on. %s`, verdictSchemaLine)
	}

	if verdict == string(OutcomeWait) {
		b.settle(OutcomeWait, "")
		return verdictResult(true, OutcomeWait,
			fmt.Sprintf("verdict recorded for ask %s: WAIT — nothing was executed; the question stays with a human. The contract is complete: do not call submit_verdict again.", b.askID)), taskengine.DataTypeJSON, nil
	}

	if err := p.answerer.Answer(ctx, b.askID, answer); err != nil {
		var refused *AnswerRefusedError
		if errors.As(err, &refused) {
			// A plain policy denial, nothing more; the contract stays open. A
			// retry yields the same denial — self-limiting within the budgets.
			return corrective("answer denied per policy for ask %s.", b.askID)
		}
		// Transient plumbing: surfaced as the tool result by the engine, so
		// the model may retry within the chain's budgets.
		return nil, taskengine.DataTypeAny, fmt.Errorf("oracletools: deliver answer for ask %s: %w", b.askID, err)
	}
	b.settle(OutcomeAnswered, answer)
	return verdictResult(true, OutcomeAnswered,
		fmt.Sprintf(`verdict recorded for ask %s: ANSWER delivered as agent "oracle"; the asking unit continues with it. The contract is complete: do not call submit_verdict again.`, b.askID)), taskengine.DataTypeJSON, nil
}

// correctionText is the machine-register nudge the gate renders while the
// contract is open — fed back to the model as its next input when it replied
// with chat text instead of a tool call.
func correctionText(b *AskBinding) string {
	return fmt.Sprintf("output rejected: submit the verdict via submit_verdict. Chat text is not a protocol action.\n\nINPUT:\n%s", b.input)
}

// execVerdictState is the deterministic gate payload: {"state","text"}. The
// chain's corrective task renders it through an output template — "settled"
// ends the chain, anything else loops the correction back to the model.
func execVerdictState(b *AskBinding) map[string]any {
	if b.settled() {
		return map[string]any{"state": verdictStateSettled, "text": ""}
	}
	return map[string]any{"state": verdictStateOpen, "text": correctionText(b)}
}

func submitVerdictToolSchema() taskengine.Tool {
	return taskengine.Tool{
		Type: "function",
		Function: taskengine.FunctionTool{
			Name:        ToolNameSubmitVerdict,
			Description: `Submit the verdict for the ask under review. Exactly one accepted call ends the review. Returns {accepted, outcome, message}: accepted false means nothing was recorded and message states what to correct.`,
			Parameters:  verdictToolParameters(),
		},
	}
}

// argString reads a string argument by key from either call shape — the
// model-driven map input, or the deterministic Args map (missiontools'
// idiom). The model shape wins when present.
func argString(input any, call *taskengine.ToolsCall, key string) string {
	if m, ok := input.(map[string]any); ok {
		if v, ok := m[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
			return fmt.Sprintf("%v", v)
		}
	}
	if call != nil && call.Args != nil {
		if v, ok := call.Args[key]; ok {
			return v
		}
	}
	return ""
}

var _ taskengine.ToolsRepo = (*provider)(nil)
