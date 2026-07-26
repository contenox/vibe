package tooleval

import "context"

// Role is a conversation role in the harness's transport-neutral message shape.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolSpec is one advertised tool, named as the MODEL sees it: provider-namespaced
// ("local_fs.list_dir"), mirroring taskenv.go's `tool.Function.Name = toolsName +
// "." + tool.Function.Name`. Advertising the production-shaped name means a real
// model behaves here as it would in serve/acp.
type ToolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters,omitempty"`
}

// ToolCall is one tool invocation the model requested. Arguments is the RAW string
// the model emitted. Whether it parses as JSON is the format-compliance axis, scored
// by the runner and never repaired here — repair belongs to localtools (rec 6).
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message is one conversation turn in the loop's running history.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Assistant is one model turn: narration plus zero or more tool calls. Zero tool
// calls ends the agentic loop (the model considers itself done).
type Assistant struct {
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Model is THE swappable seam (Verify: "design the seam so the engine-driving path
// is swappable"). Given the running conversation and the advertised tools, Turn
// returns the model's next turn. engineModel drives a configured real model; the
// scripted responders below drive the hermetic self-test. The runner owns the loop
// and the real tools around this one interface.
type Model interface {
	// Name identifies the model in the report matrix ("qwen2.5:0.5b", "scripted").
	Name() string
	Turn(ctx context.Context, convo []Message, tools []ToolSpec) (Assistant, error)
}

// ScriptedModel replays a fixed sequence of turns, one per call, then falls silent
// (an empty Assistant — no tool calls — which ends the loop). It is the deterministic
// responder the hermetic self-test uses to prove the loop/tools/scoring PLUMBING
// without a model. A real model is stochastic on purpose; this is the opposite on
// purpose.
type ScriptedModel struct {
	name  string
	turns []Assistant
	i     int
}

// NewScriptedModel returns a ScriptedModel that emits turns in order.
func NewScriptedModel(name string, turns ...Assistant) *ScriptedModel {
	return &ScriptedModel{name: name, turns: turns}
}

func (m *ScriptedModel) Name() string { return m.name }

func (m *ScriptedModel) Turn(_ context.Context, _ []Message, _ []ToolSpec) (Assistant, error) {
	if m.i >= len(m.turns) {
		return Assistant{Content: "done"}, nil
	}
	t := m.turns[m.i]
	m.i++
	return t, nil
}

// FuncModel computes each turn from the live conversation. It lets a self-test
// script guidance-REACTIVE behaviour (e.g. stop repeating a call once a "[harness]"
// marker appears in the last tool result), so scenario 2's A/B delta can be
// exercised deterministically end to end.
type FuncModel struct {
	name string
	fn   func(convo []Message, tools []ToolSpec) Assistant
}

// NewFuncModel wraps fn as a Model.
func NewFuncModel(name string, fn func(convo []Message, tools []ToolSpec) Assistant) *FuncModel {
	return &FuncModel{name: name, fn: fn}
}

func (m *FuncModel) Name() string { return m.name }

func (m *FuncModel) Turn(_ context.Context, convo []Message, tools []ToolSpec) (Assistant, error) {
	return m.fn(convo, tools), nil
}
