package taskengine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/contenox/contenox/internal/kernel/taskengine/llmretry"
	"gopkg.in/yaml.v3"
)

// TaskHandler defines how task outputs are processed and interpreted.
type TaskHandler string

const (
	HandleRaiseError       TaskHandler = "raise_error"
	HandleRoute            TaskHandler = "route"
	HandleChatCompletion   TaskHandler = "chat_completion"
	HandleExecuteToolCalls TaskHandler = "execute_tool_calls"
	HandleNoop             TaskHandler = "noop"
	HandleTools            TaskHandler = "tools"
)

func (t TaskHandler) String() string {
	return string(t)
}

// IsAssistantProseHandler reports whether a TaskEventStepChunk carrying this
// handler's streamed output is user-visible assistant narration — text and
// reasoning a chat surface should render into the reply — as opposed to
// control flow that merely happens to stream (e.g. a route task's streamed
// output is its routing label, not prose). A chunk this rejects is still
// journaled, not dropped; every event translator consumes this judgement.
func IsAssistantProseHandler(handler string) bool {
	return TaskHandler(handler) == HandleChatCompletion
}

// IsToolBearingHandler reports whether this handler already reports its own
// work through the dedicated tool-call events (TaskEventToolCallPending /
// TaskEventToolCall) or a chat surface's own tool rendering. Event
// translators use it to suppress the generic step-lifecycle card
// (TaskEventStepStarted / StepCompleted / StepFailed) for those handlers, so
// one action is not double-rendered as both a step card and a tool card.
func IsToolBearingHandler(handler string) bool {
	switch TaskHandler(handler) {
	case HandleExecuteToolCalls, HandleTools, HandleChatCompletion, HandleRoute:
		return true
	default:
		return false
	}
}

// Transition-eval tokens are the control values a handler emits as its
// transition "eval"; a TransitionBranch matches them via its When field (with
// the default Operator, exact string equality). These are part of the DSL
// contract — branch on these constants, not the model's free text:
//
//   - chat_completion        → TransitionToolCall (model requested tools) | TransitionExecuted (finished, no tool calls)
//   - execute_tool_calls     → TransitionNoop (empty history) | TransitionNoCallsFound (model produced no tool calls) | TransitionToolsExecuted | TransitionFailed
//   - tools                  → TransitionToolsExecuted | TransitionFailed (or, when OutputTemplate is set, its rendered text)
//   - noop                   → TransitionNoop
//
// To branch on the model's actual text, use the `route` handler, whose eval IS
// the model's chosen label.
const (
	// TransitionExecuted: a chat_completion turn finished with no tool calls.
	TransitionExecuted = "executed"
	// TransitionToolCall: a chat_completion turn requested one or more tool
	// calls. Snake_case to match the "tool_call" task-event kind.
	TransitionToolCall = "tool_call"
	// TransitionNoop: the noop handler ran, or execute_tool_calls saw empty history.
	TransitionNoop = "noop"
	// TransitionNoCallsFound: the model's last message carried no tool calls to run.
	TransitionNoCallsFound = "no_calls_found"
	// TransitionToolsExecuted: a tools task ran its tool successfully.
	TransitionToolsExecuted = "tools_executed"
	// TransitionFailed: a tools task failed.
	TransitionFailed = "failed"
)

// DataType (un)marshals as its lowercase string name in both JSON and YAML.
// All four methods route through String()/DataTypeFromString so the two
// parsers never drift.

func (d DataType) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

func (d DataType) MarshalYAML() (any, error) {
	return d.String(), nil
}

func (dt *DataType) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v, err := DataTypeFromString(s)
	if err != nil {
		return err
	}
	*dt = v
	return nil
}

func (dt *DataType) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	v, err := DataTypeFromString(s)
	if err != nil {
		return err
	}
	*dt = v
	return nil
}

// TaskTransition defines what happens after a task completes,
// including which task to go to next and how to handle errors.
type TaskTransition struct {
	// OnFailure is the task ID to jump to in case of failure.
	OnFailure string `yaml:"on_failure" json:"on_failure" example:"error_handler"`

	// Branches defines conditional branches for successful task completion.
	Branches []TransitionBranch `yaml:"branches" json:"branches" openapi_include_type:"taskengine.TransitionBranch"`
}

// TransitionBranch defines a single possible path in the workflow,
// selected when the task's output matches the specified condition.
type TransitionBranch struct {
	// Operator defines how to compare the task's transition eval to When. It is
	// REQUIRED and must be one of SupportedOperators() — an empty or unknown
	// operator is rejected at chain validation (at runtime it would never match,
	// a silent dead branch). The comparison for equals/contains/starts_with/
	// ends_with is byte-exact and CASE-SENSITIVE with no trimming — a trailing
	// newline (common in multiline template literals) will not match. Only the
	// `route` handler normalizes its answer.
	Operator OperatorTerm `yaml:"operator,omitempty" json:"operator,omitempty" example:"equals" openapi_include_type:"string"`

	// When is the value this branch matches against the task's transition eval.
	// What the eval is depends on the handler:
	//   - chat_completion / execute_tool_calls / tools / noop → a control token,
	//     one of the Transition* constants (e.g. "tool_call", "executed",
	//     "tools_executed", "no_calls_found", "noop", "failed"). You CANNOT branch
	//     on the model's free text here — use the `route` handler for that.
	//   - route → the model's chosen label (one of the declared branch targets).
	//   - edge_traversed_at_least → an integer threshold (see that operator).
	When string `yaml:"when" json:"when" example:"tool_call"`

	// Goto specifies the target task ID if this branch is taken.
	// Leave empty or use taskengine.TermEnd to end the chain.
	Goto string `yaml:"goto" json:"goto" example:"positive_response"`

	// Edge identifies a graph edge "fromTaskID->toTaskID" whose traversal
	// count is consulted by edge-state operators (e.g. edge_traversed_at_least).
	// Required when Operator is one of those; ignored otherwise.
	Edge string `yaml:"edge,omitempty" json:"edge,omitempty" example:"chat->run_tools"`
}

// OperatorTerm represents logical operators used for task transition evaluation
type OperatorTerm string

const (
	OpEquals     OperatorTerm = "equals"
	OpContains   OperatorTerm = "contains"
	OpStartsWith OperatorTerm = "starts_with"
	OpEndsWith   OperatorTerm = "ends_with"
	OpDefault    OperatorTerm = "default"
	// OpEdgeTraversedAtLeast fires when the edge specified by TransitionBranch.Edge
	// (formatted "fromTaskID->toTaskID") has been traversed at least the integer
	// in TransitionBranch.When times during the current chain run. Reads engine
	// state, not task output. Use it to bound workflow loops:
	//
	//   { "operator": "edge_traversed_at_least",
	//     "edge": "chat->run_tools", "when": "20", "goto": "summarise_failure" }
	//
	// Place this branch ahead of the normal loop branch so it intercepts before
	// the next loop iteration fires.
	OpEdgeTraversedAtLeast OperatorTerm = "edge_traversed_at_least"
)

func (t OperatorTerm) String() string {
	return string(t)
}

func SupportedOperators() []string {
	return []string{
		string(OpEquals),
		string(OpContains),
		string(OpStartsWith),
		string(OpEndsWith),
		string(OpDefault),
		string(OpEdgeTraversedAtLeast),
	}
}

func ToOperatorTerm(s string) (OperatorTerm, error) {
	switch s {
	case string(OpEquals):
		return OpEquals, nil
	case string(OpContains):
		return OpContains, nil
	case string(OpStartsWith):
		return OpStartsWith, nil
	case string(OpEndsWith):
		return OpEndsWith, nil
	case string(OpDefault):
		return OpDefault, nil
	case string(OpEdgeTraversedAtLeast):
		return OpEdgeTraversedAtLeast, nil
	default:
		return "", fmt.Errorf("unsupported operator: %s", s)
	}
}

// LLMExecutionConfig represents configuration for executing tasks using Large Language Models (LLMs).
type LLMExecutionConfig struct {
	// Model is the primary model: it is placed first in the candidate list and is
	// the model used for token counting (see GetPrimaryModel). When both Model and
	// Models are set, Model plus Models form the candidate set (Model first);
	// the resolver then picks a reachable one — so set exactly Model for a single
	// pinned model, or use Models for an explicit candidate pool.
	Model string `yaml:"model" json:"model" example:"llama2:7b"`
	// Models is an additional candidate pool, considered alongside Model.
	Models []string `yaml:"models,omitempty" json:"models,omitempty" example:"[\"gpt-4\", \"gpt-3.5-turbo\"]"`
	// Provider is the primary provider, placed first in the candidate list;
	// Providers supplies additional candidates.
	Provider  string   `yaml:"provider,omitempty" json:"provider,omitempty" example:"ollama"`
	Providers []string `yaml:"providers,omitempty" json:"providers,omitempty" example:"[\"ollama\", \"openai\"]"`
	// Temperature is the sampling temperature; pointer so "unset" (nil) is
	// distinguishable from an explicit 0.0. When set it is honored everywhere.
	// When unset: chat_completion uses the provider default; the prompt/route
	// handlers use 0.0 (route depends on deterministic single-label output).
	Temperature *float32 `yaml:"temperature,omitempty" json:"temperature,omitempty" example:"0.7"`
	// Tools is the allowlist of registry tool names this task may invoke
	// (client-passed tools are governed separately by PassClientsTools):
	// absent/null/[] exposes none; ["*"] exposes all; ["a","b"] exposes only
	// those names (unknown ones ignored); ["*","!name"] excludes name(s),
	// meaningful only combined with "*".
	Tools []string `yaml:"tools,omitempty" json:"tools,omitempty" example:"[\"local_shell\", \"nws\"]"`
	// HideTools suppresses specific tools by (namespaced) name from BOTH the
	// registry tools selected via Tools and the client-passed tools.
	HideTools []string `yaml:"hide_tools,omitempty" json:"hide_tools,omitempty" example:"[\"tool1\", \"tools_name1.tool1\"]"`
	// ToolsPolicies carries per-tools policy overrides for this task (tools
	// name -> policy key -> value), injected into the context before
	// GetToolsForToolsByName so tools can render dynamic descriptions and
	// enforce the policy at Exec time.
	ToolsPolicies    map[string]map[string]string `yaml:"tools_policies,omitempty" json:"tools_policies,omitempty"`
	PassClientsTools bool                         `yaml:"pass_clients_tools" json:"pass_clients_tools"`
	// Think controls reasoning mode: auto, off, minimal, low, medium, high,
	// xhigh, or a boolean-style alias. Empty uses the provider default.
	Think string `yaml:"think,omitempty" json:"think,omitempty" example:"high"`
	// MaxTokens caps the model's output tokens. Unset sends no explicit cap
	// and never falls back to the chain's TokenLimit, which bounds the
	// input+output window, not the output alone (conflating the two trips
	// per-model output limits, e.g. Vertex Gemini 2.5 Pro's 65536 cap).
	MaxTokens *int `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty" example:"8192"`
	// MaxTokensTemplate stores a string max_tokens macro from chain JSON until
	// MacroEnv expands it into MaxTokens. It is not emitted as a separate field.
	MaxTokensTemplate string `yaml:"-" json:"-"`
	// Shift allows the context window to slide on overflow instead of erroring.
	Shift bool `yaml:"shift,omitempty" json:"shift,omitempty"`
	// RetryPolicy wraps the underlying chat/prompt call with classified retry
	// (rate-limit / server-error / timeout) and an optional model fallback.
	// Nil or zero-value disables retry — current default. See [llmretry.Do].
	RetryPolicy *llmretry.RetryPolicy `yaml:"retry_policy,omitempty" json:"retry_policy,omitempty"`
}

func (c *LLMExecutionConfig) UnmarshalJSON(data []byte) error {
	type noMethods LLMExecutionConfig
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}

	maxTokensRaw, hasMaxTokens := fields["max_tokens"]
	delete(fields, "max_tokens")

	rest, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	var out noMethods
	if err := json.Unmarshal(rest, &out); err != nil {
		return err
	}
	*c = LLMExecutionConfig(out)

	if !hasMaxTokens {
		return nil
	}
	return c.unmarshalMaxTokens(maxTokensRaw)
}

func (c *LLMExecutionConfig) UnmarshalYAML(value *yaml.Node) error {
	type noMethods LLMExecutionConfig
	if value == nil {
		return nil
	}

	var maxTokensNode *yaml.Node
	decodeNode := value
	if value.Kind == yaml.MappingNode {
		clone := *value
		clone.Content = make([]*yaml.Node, 0, len(value.Content))
		for i := 0; i+1 < len(value.Content); i += 2 {
			key, val := value.Content[i], value.Content[i+1]
			if key.Value == "max_tokens" {
				maxTokensNode = val
				continue
			}
			clone.Content = append(clone.Content, key, val)
		}
		decodeNode = &clone
	}

	var out noMethods
	if err := decodeNode.Decode(&out); err != nil {
		return err
	}
	*c = LLMExecutionConfig(out)

	if maxTokensNode == nil {
		return nil
	}
	return c.unmarshalMaxTokensYAML(maxTokensNode)
}

func (c LLMExecutionConfig) MarshalJSON() ([]byte, error) {
	type noMethods LLMExecutionConfig
	data, err := json.Marshal(noMethods(c))
	if err != nil {
		return nil, err
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if strings.TrimSpace(c.MaxTokensTemplate) != "" {
		b, err := json.Marshal(c.MaxTokensTemplate)
		if err != nil {
			return nil, err
		}
		fields["max_tokens"] = b
	}
	return json.Marshal(fields)
}

func (c *LLMExecutionConfig) unmarshalMaxTokens(raw json.RawMessage) error {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		c.MaxTokens = nil
		c.MaxTokensTemplate = ""
		return nil
	}

	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		c.MaxTokens = &n
		c.MaxTokensTemplate = ""
		return nil
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			c.MaxTokens = nil
			c.MaxTokensTemplate = ""
			return nil
		}
		if parsed, err := strconv.Atoi(s); err == nil {
			c.MaxTokens = &parsed
			c.MaxTokensTemplate = ""
			return nil
		}
		c.MaxTokens = nil
		c.MaxTokensTemplate = s
		return nil
	}

	return fmt.Errorf("max_tokens must be an integer or string macro")
}

func (c *LLMExecutionConfig) unmarshalMaxTokensYAML(value *yaml.Node) error {
	if value == nil || value.Tag == "!!null" {
		c.MaxTokens = nil
		c.MaxTokensTemplate = ""
		return nil
	}

	var n int
	if err := value.Decode(&n); err == nil {
		c.MaxTokens = &n
		c.MaxTokensTemplate = ""
		return nil
	}

	var s string
	if err := value.Decode(&s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			c.MaxTokens = nil
			c.MaxTokensTemplate = ""
			return nil
		}
		if parsed, err := strconv.Atoi(s); err == nil {
			c.MaxTokens = &parsed
			c.MaxTokensTemplate = ""
			return nil
		}
		c.MaxTokens = nil
		c.MaxTokensTemplate = s
		return nil
	}

	return fmt.Errorf("max_tokens must be an integer or string macro")
}

// ToolsCall configures a `tools` task — a direct, deterministic call to one tool
// of one registered tools-provider (e.g. an MCP server), distinct from the
// model-driven tool calls of chat_completion/execute_tool_calls.
type ToolsCall struct {
	// Name is the registered tools-PROVIDER (the service/server, e.g. "slack"),
	// not the tool. Required.
	Name string `yaml:"name" json:"name" example:"slack"`

	// ToolName is the specific TOOL to invoke on that provider
	// (e.g. "send_slack_notification").
	ToolName string `yaml:"tool_name" json:"tool_name" example:"send_slack_notification"`
	// Args are key-value pairs passed to the tool call.
	Args map[string]string `yaml:"args,omitempty" json:"args,omitempty" example:"{\"channel\": \"#alerts\", \"message\": \"Task completed successfully\"}"`
}

type TaskDefinition struct {
	ID string `yaml:"id" json:"id" example:"validate_input"`

	Description string `yaml:"description" json:"description" example:"Validates user input meets quality requirements"`

	// Handler determines how the LLM output (or tools) will be interpreted.
	Handler TaskHandler `yaml:"handler" json:"handler" example:"chat_completion" openapi_include_type:"string"`

	SystemInstruction string `yaml:"system_instruction,omitempty" json:"system_instruction,omitempty" example:"You are a quality control assistant. Respond only with 'valid' or 'invalid'."`

	ExecuteConfig *LLMExecutionConfig `yaml:"execute_config,omitempty" json:"execute_config,omitempty" openapi_include_type:"taskengine.LLMExecutionConfig"`

	// Tools defines an external action to run: required for Tools tasks,
	// must be nil/omitted for all other types.
	Tools *ToolsCall `yaml:"tools,omitempty" json:"tools,omitempty" openapi_include_type:"taskengine.ToolsCall"`

	// Print optionally formats the output for display/logging, supporting
	// template variables from previous task outputs.
	Print string `yaml:"print,omitempty" json:"print,omitempty" example:"Validation result: {{.validate_input}}"`

	// PromptTemplate, when set, overrides the resolved input as the prompt
	// sent to the LLM. Supports template variables from previous task outputs.
	PromptTemplate string `yaml:"prompt_template,omitempty" json:"prompt_template,omitempty" example:"Is this input valid? {{.input}}"`

	// OutputTemplate, when set, renders a tools task's JSON output through
	// this go template; the rendered string becomes the task's output.
	OutputTemplate string `yaml:"output_template,omitempty" json:"output_template,omitempty" example:"Tools result: {{.status}}"`

	// InputVar names the variable to use as this task's input; each task
	// stores its own output in a variable named after its task id.
	InputVar string `yaml:"input_var,omitempty" json:"input_var,omitempty" example:"input"`

	// InputMaxBytes caps oversized string/chat-history inputs before this task
	// runs, for recovery/summarization tasks that should explain a failure
	// without re-feeding the huge input that caused it.
	InputMaxBytes int `yaml:"input_max_bytes,omitempty" json:"input_max_bytes,omitempty" example:"8192"`

	// Transition defines what to do after this task completes.
	Transition TaskTransition `yaml:"transition" json:"transition" openapi_include_type:"taskengine.TaskTransition"`

	// Timeout is a Go duration string ("10s", "2m") bounding task execution.
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty" example:"30s"`

	// RetryOnFailure sets how many times to retry this task on failure (all
	// task types); 0 means no retries.
	RetryOnFailure int `yaml:"retry_on_failure,omitempty" json:"retry_on_failure,omitempty" example:"2"`
}

type ChainTerms string

const (
	TermEnd = "end"
)

// TaskChainDefinition describes a sequence of tasks to execute in order,
// with branching logic, retry policies, and model preferences.
type TaskChainDefinition struct {
	ID string `yaml:"id" json:"id"`

	// Debug enables capturing user input and output.
	Debug bool `yaml:"debug" json:"debug"`

	Description string `yaml:"description" json:"description"`

	Tasks []TaskDefinition `yaml:"tasks" json:"tasks" openapi_include_type:"taskengine.TaskDefinition"`

	// TokenLimit is the token limit for the context window used during execution.
	TokenLimit int64 `yaml:"token_limit" json:"token_limit"`
}

// ChatHistory represents a conversation history with an LLM.
type ChatHistory struct {
	// Messages is the list of messages in the conversation.
	Messages []Message `json:"messages"`
	// Model is the name of the model to use for the conversation.
	Model string `json:"model" example:"llama2:7b"`
	// InputTokens will be filled by the engine and will hold the number of tokens used for the input.
	InputTokens int `json:"inputTokens" example:"15"`
	// OutputTokens will be filled by the engine and will hold the number of tokens used for the output.
	OutputTokens int `json:"outputTokens" example:"10"`
	// FinishReason is the provider's verbatim finish reason for the LAST model
	// call that produced this history ("length"-class values mean the response
	// was truncated). Filled by the engine; empty when no model call ran or
	// the provider reported none.
	FinishReason string `json:"finishReason,omitempty" example:"stop"`
}

// Message represents a single message in a chat conversation.
type Message struct {
	// ID is not used by the engine; it is useful for tracking messages and
	// computing differences of histories before storage.
	ID      string `json:"id" example:"msg_123456"`
	Role    string `json:"role" example:"user"`
	Content string `json:"content,omitempty" example:"What is the capital of France?"`
	// Images travel with Content to the model; requests with images
	// resolve only to vision-capable models and fail with a typed error
	// when none is available.
	Images []ImagePart `json:"images,omitempty" openapi_include_type:"taskengine.ImagePart"`
	// Thinking is the model's internal reasoning trace, populated only when
	// thinking is enabled; never sent back to the model as history.
	Thinking   string     `json:"thinking,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	CallTools  []ToolCall `json:"callTools,omitempty"`
	Timestamp  time.Time  `json:"timestamp" example:"2023-11-15T14:30:45Z"`
	// RequestID and ChainRef are turn provenance (the run's X-Request-ID and
	// the chain path that ran it), not used by the engine.
	RequestID string `json:"requestId,omitempty"`
	ChainRef  string `json:"chainRef,omitempty"`
}

// ImagePart is a binary image attachment on a Message.
type ImagePart struct {
	// Data is the raw image bytes. JSON encoding carries it as standard base64.
	Data []byte `json:"data"`
	// MimeType is the image media type.
	MimeType string `json:"mime_type" example:"image/png"`
}

// Tool represents a tool that can be called by the model.
type Tool struct {
	Type     string       `json:"type"`
	Function FunctionTool `json:"function"`
}

// FunctionTool defines the schema for a function-type tool.
type FunctionTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"` // JSON Schema object
}

// ToolCall represents a tool call requested by the model.
type ToolCall struct {
	ID       string       `json:"id" example:"call_abc123"`
	Type     string       `json:"type" example:"function"`
	Function FunctionCall `json:"function" openapi_include_type:"taskengine.FunctionCall"`
	// ProviderMeta carries opaque provider-specific data (e.g. Gemini thought_signature)
	// that must be round-tripped back on the next turn.
	ProviderMeta map[string]string `json:"provider_meta,omitempty" example:"{\"thought_signature\":\"123456\"}"`
}

// FunctionCall specifies the function name and arguments for a tool call.
type FunctionCall struct {
	Name      string `json:"name" example:"get_current_weather"`
	Arguments string `json:"arguments" example:"{\n  \"location\": \"San Francisco, CA\",\n  \"unit\": \"celsius\"\n}"`
}

type FunctionCallObject struct {
	Name      string `json:"name" example:"get_current_weather"`
	Arguments any    `json:"arguments"`
}
