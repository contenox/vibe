package taskengine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/contenox/contenox/runtime/taskengine/compact"
	"github.com/contenox/contenox/runtime/taskengine/llmretry"
	"gopkg.in/yaml.v3"
)


// TaskHandler defines how task outputs are processed and interpreted.
type TaskHandler string

const (
	HandlePromptToString TaskHandler = "prompt_to_string"
	HandlePromptToInt    TaskHandler = "prompt_to_int"
	HandleRaiseError     TaskHandler = "raise_error"
	HandleChatCompletion TaskHandler = "chat_completion"
	HandleExecuteToolCalls TaskHandler = "execute_tool_calls"
	HandleNoop TaskHandler = "noop"
	HandleHook TaskHandler = "hook"
)

func (t TaskHandler) String() string {
	return string(t)
}

func (d DataType) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

func (d DataType) MarshalYAML() ([]byte, error) {
	return yaml.Marshal(d.String())
}

func (dt *DataType) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}

	switch strings.ToLower(s) {
	case "any":
		*dt = DataTypeAny
	case "string":
		*dt = DataTypeString
	case "int":
		*dt = DataTypeInt
	case "json":
		*dt = DataTypeJSON
	case "chat_history":
		*dt = DataTypeChatHistory
	default:
		return fmt.Errorf("unknown data type: %q", s)
	}

	return nil
}

func (dt *DataType) UnmarshalYAML(data []byte) error {
	var s string
	if err := yaml.Unmarshal(data, &s); err != nil {
		return err
	}

	switch strings.ToLower(s) {
	case "string":
		*dt = DataTypeString
	case "int":
		*dt = DataTypeInt
	case "json":
		*dt = DataTypeJSON
	case "chat_history":
		*dt = DataTypeChatHistory
	default:
		return fmt.Errorf("unknown data type: %q", s)
	}

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
	// Operator defines how to compare the task's output to When.
	Operator OperatorTerm `yaml:"operator,omitempty" json:"operator,omitempty" example:"equals" openapi_include_type:"string"`

	// When specifies the condition that must be met to follow this branch.
	// Format depends on the task type:
	// - For condition_key: exact string match
	// - For prompt_to_int: numeric comparison (using Operator)
	When string `yaml:"when" json:"when" example:"yes"`

	// Goto specifies the target task ID if this branch is taken.
	// Leave empty or use taskengine.TermEnd to end the chain.
	Goto string `yaml:"goto" json:"goto" example:"positive_response"`

	// Compose defines how to transform data when taking this branch.
	// Optional - if not specified, the current task output is passed as-is.
	Compose *BranchCompose `yaml:"compose,omitempty" json:"compose,omitempty" openapi_include_type:"taskengine.BranchCompose"`
}

// OperatorTerm represents logical operators used for task transition evaluation
type OperatorTerm string

const (
	OpEquals      OperatorTerm = "equals"
	OpContains    OperatorTerm = "contains"
	OpStartsWith  OperatorTerm = "starts_with"
	OpEndsWith    OperatorTerm = "ends_with"
	OpGreaterThan OperatorTerm = ">"
	OpGt          OperatorTerm = "gt"
	OpLessThan    OperatorTerm = "<"
	OpLt          OperatorTerm = "lt"
	OpInRange     OperatorTerm = "in_range"
	OpDefault     OperatorTerm = "default"
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
		string(OpGreaterThan),
		string(OpGt),
		string(OpLessThan),
		string(OpLt),
		string(OpInRange),
		string(OpDefault),
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
	case string(OpGreaterThan):
		return OpGreaterThan, nil
	case string(OpGt):
		return OpGt, nil
	case string(OpLessThan):
		return OpLessThan, nil
	case string(OpLt):
		return OpLt, nil
	case string(OpInRange):
		return OpInRange, nil
	case string(OpDefault):
		return OpDefault, nil
	default:
		return "", fmt.Errorf("unsupported operator: %s", s)
	}
}

// LLMExecutionConfig represents configuration for executing tasks using Large Language Models (LLMs).
type LLMExecutionConfig struct {
	Model            string   `yaml:"model" json:"model" example:"mistral:instruct"`
	Models           []string `yaml:"models,omitempty" json:"models,omitempty" example:"[\"gpt-4\", \"gpt-3.5-turbo\"]"`
	Provider         string   `yaml:"provider,omitempty" json:"provider,omitempty" example:"ollama"`
	Providers        []string `yaml:"providers,omitempty" json:"providers,omitempty" example:"[\"ollama\", \"openai\"]"`
	Temperature      float32  `yaml:"temperature,omitempty" json:"temperature,omitempty" example:"0.7"`
	// Hooks is the allowlist of hook names this task may invoke.
	//
	// Patterns supported:
	//   - absent/null   — all registered hooks (backward-compatible default)
	//   - []            — no hooks exposed to the model
	//   - ["*"]         — all registered hooks (explicit)
	//   - ["a","b"]     — only the named hooks (unknown names silently ignored)
	//   - ["*","!name"] — all hooks except the excluded name(s)
	//
	// Exclusions ("!name") are only meaningful when combined with "*".
	Hooks     []string `yaml:"hooks,omitempty" json:"hooks,omitempty" example:"[\"local_shell\", \"nws\"]"`
	HideTools []string `yaml:"hide_tools,omitempty" json:"hide_tools,omitempty" example:"[\"tool1\", \"hook_name1.tool1\"]"`
	// HookPolicies carries per-hook policy overrides for this task.
	// Keys are hook names; values are maps of policy key → value pairs.
	// These are injected into the context before GetToolsForHookByName is called,
	// so hooks can produce dynamic tool descriptions and enforce the policy at Exec time.
	//
	// Example (local_shell):
	//   hook_policies:
	//     local_shell:
	//       _allowed_commands: "git,go,ls,cat,grep"
	//       _denied_commands:  "sudo,su,dd,mkfs"
	HookPolicies     map[string]map[string]string `yaml:"hook_policies,omitempty" json:"hook_policies,omitempty"`
	PassClientsTools bool                         `yaml:"pass_clients_tools" json:"pass_clients_tools"`
	// Think enables reasoning mode for supported models.
	// Accepts "true"/"false" or "high"/"medium"/"low". Empty = provider default (off).
	Think string `yaml:"think,omitempty" json:"think,omitempty" example:"high"`
	// Shift allows the context window to slide on overflow instead of erroring.
	Shift bool `yaml:"shift,omitempty" json:"shift,omitempty"`
	// RetryPolicy wraps the underlying chat/prompt call with classified retry
	// (rate-limit / server-error / timeout) and an optional model fallback.
	// Nil or zero-value disables retry — current default. See [llmretry.Do].
	RetryPolicy *llmretry.RetryPolicy `yaml:"retry_policy,omitempty" json:"retry_policy,omitempty"`
	// CompactPolicy enables mid-run conversation compaction at the head of a
	// chat_completion task: when the running ChatHistory exceeds
	// TriggerFraction * token_limit, older messages are summarized and replaced
	// with a single synthetic <compact-summary> user message. Nil disables
	// compaction (current default). See [compact.Maybe].
	CompactPolicy *compact.Policy `yaml:"compact_policy,omitempty" json:"compact_policy,omitempty"`
}

// HookCall represents an external integration or side-effect triggered during a task.
// Hooks allow tasks to interact with external systems (e.g., "send_email", "update_db").
type HookCall struct {
	// Name is the registered hook-service (e.g., "send_email").
	Name string `yaml:"name" json:"name" example:"slack"`

	// ToolName is the name of the tool to invoke (e.g., "send_slack_notification").
	ToolName string `yaml:"tool_name" json:"tool_name" example:"send_slack_notification"`
	// Args are key-value pairs to parameterize the hook call.
	// Example: {"to": "user@example.com", "subject": "Notification"}
	Args map[string]string `yaml:"args" json:"args" example:"{\"channel\": \"#alerts\", \"message\": \"Task completed successfully\"}"`
}

type TaskDefinition struct {
	// ID uniquely identifies the task within the chain.
	ID string `yaml:"id" json:"id" example:"validate_input"`

	// Description is a human-readable summary of what the task does.
	Description string `yaml:"description" json:"description" example:"Validates user input meets quality requirements"`

	// Handler determines how the LLM output (or hook) will be interpreted.
	Handler TaskHandler `yaml:"handler" json:"handler" example:"condition_key" openapi_include_type:"string"`

	// SystemInstruction provides additional instructions to the LLM, if applicable system level will be used.
	SystemInstruction string `yaml:"system_instruction,omitempty" json:"system_instruction,omitempty" example:"You are a quality control assistant. Respond only with 'valid' or 'invalid'."`

	// ExecuteConfig defines the configuration for executing prompt or chat model tasks.
	ExecuteConfig *LLMExecutionConfig `yaml:"execute_config,omitempty" json:"execute_config,omitempty" openapi_include_type:"taskengine.LLMExecutionConfig"`

	// Hook defines an external action to run.
	// Required for Hook tasks, must be nil/omitted for all other types.
	// Example: {type: "send_email", args: {"to": "user@example.com"}}
	Hook *HookCall `yaml:"hook,omitempty" json:"hook,omitempty" openapi_include_type:"taskengine.HookCall"`

	// Print optionally formats the output for display/logging.
	// Supports template variables from previous task outputs.
	// Optional for all task types except Hook where it's rarely used.
	// Example: "The score is: {{.previous_output}}"
	Print string `yaml:"print,omitempty" json:"print,omitempty" example:"Validation result: {{.validate_input}}"`

	// PromptTemplate is the text prompt sent to the LLM.
	// It's Required and only applicable for the prompt_to_string type.
	// Supports template variables from previous task outputs.
	// Example: "Rate the quality from 1-10: {{.input}}"
	PromptTemplate string `yaml:"prompt_template" json:"prompt_template" example:"Is this input valid? {{.input}}"`

	// OutputTemplate is an optional go template to format the output of a hook.
	// If specified, the hook's JSON output will be used as data for the template.
	// The final output of the task will be the rendered string.
	// Example: "The weather is {{.weather}} with a temperature of {{.temperature}}."
	OutputTemplate string `yaml:"output_template,omitempty" json:"output_template,omitempty" example:"Hook result: {{.status}}"`

	// InputVar is the name of the variable to use as input for the task.
	// Example: "input" for the original input.
	// Each task stores its output in a variable named with it's task id.
	InputVar string `yaml:"input_var,omitempty" json:"input_var,omitempty" example:"input"`

	// Transition defines what to do after this task completes.
	Transition TaskTransition `yaml:"transition" json:"transition" openapi_include_type:"taskengine.TaskTransition"`

	// Timeout optionally sets a timeout for task execution.
	// Format: "10s", "2m", "1h" etc.
	// Optional for all task types.
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty" example:"30s"`

	// RetryOnFailure sets how many times to retry this task on failure.
	// Applies to all task types including Hooks.
	// Default: 0 (no retries)
	RetryOnFailure int `yaml:"retry_on_failure,omitempty" json:"retry_on_failure,omitempty" example:"2"`
}

// BranchCompose is a task that composes multiple variables into a single output.
// the composed output is stored in a variable named after the task ID with "_composed" suffix.
// and is also directly mutating the task's output.
// example:
//
// compose:
//
//	with_var: "chat2"
//	strategy: "override"
type BranchCompose struct {
	// Selects the variable to compose the current input with.
	WithVar string `yaml:"with_var,omitempty" json:"with_var,omitempty"`
	// Strategy defines how values should be merged ("override", "merge_chat_histories", "append_string_to_chat_history").
	// Optional; defaults to "override" or "merge_chat_histories" if both output and WithVar values are ChatHistory.
	// "merge_chat_histories": If both output and WithVar values are ChatHistory,
	// appends the WithVar's Messages to the output's Messages.
	Strategy string `yaml:"strategy,omitempty" json:"strategy,omitempty"`
}

type ChainTerms string

const (
	TermEnd = "end"
)

// TaskChainDefinition describes a sequence of tasks to execute in order,
// along with branching logic, retry policies, and model preferences.
//
// TaskChainDefinition support dynamic routing based on LLM outputs or conditions,
// and can include hooks to perform external actions (e.g., sending emails).
type TaskChainDefinition struct {
	// ID uniquely identifies the chain.
	ID string `yaml:"id" json:"id"`

	// Enables capturing user input and output.
	Debug bool `yaml:"debug" json:"debug"`

	// Description provides a human-readable summary of the chain's purpose.
	Description string `yaml:"description" json:"description"`

	// Tasks is the list of tasks to execute in sequence.
	Tasks []TaskDefinition `yaml:"tasks" json:"tasks" openapi_include_type:"taskengine.TaskDefinition"`

	// TokenLimit is the token limit for the context window (used during execution).
	TokenLimit int64 `yaml:"token_limit" json:"token_limit"`
}

// ChatHistory represents a conversation history with an LLM.
type ChatHistory struct {
	// Messages is the list of messages in the conversation.
	Messages []Message `json:"messages"`
	// Model is the name of the model to use for the conversation.
	Model string `json:"model" example:"mistral:instruct"`
	// InputTokens will be filled by the engine and will hold the number of tokens used for the input.
	InputTokens int `json:"inputTokens" example:"15"`
	// OutputTokens will be filled by the engine and will hold the number of tokens used for the output.
	OutputTokens int `json:"outputTokens" example:"10"`
}

// Message represents a single message in a chat conversation.
type Message struct {
	// ID is the unique identifier for the message.
	// This field is not used by the engine. It can be filled as part of the Request, or left empty.
	// The ID is useful for tracking messages and computing differences of histories before storage.
	ID string `json:"id" example:"msg_123456"`
	// Role is the role of the message sender.
	Role string `json:"role" example:"user"`
	// Content is the content of the message.
	Content string `json:"content,omitempty" example:"What is the capital of France?"`
	// Thinking is the model's internal reasoning trace.
	// Only populated when thinking is enabled; never sent back to the model as history.
	Thinking string `json:"thinking,omitempty"`
	// ToolCallID is the ID of the tool call associated with the message.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// CallTools is the tool call of the message sender.
	CallTools []ToolCall `json:"callTools,omitempty"`
	// Timestamp is the time the message was sent.
	Timestamp time.Time `json:"timestamp" example:"2023-11-15T14:30:45Z"`
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

