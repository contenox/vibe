# Handlers

Every task has a `handler` field that determines what it does. This page documents all available handlers and which fields are valid for each.

## Handler types

| Handler | What it does |
|---------|-------------|
| `chat_completion` | Send messages to an LLM, receive a text/tool-call reply |
| `execute_tool_calls` | Execute the tool calls from the previous LLM reply |
| `hook` | Call a specific named hook tool directly (no LLM involved) |
| `condition_key` | Branch based on a keyword the model output |
| `prompt_to_string` | Render a Go template with task variables, output as string |
| `noop` | Pass input through unchanged |

---

## `chat_completion`

Sends the current input to the LLM and waits for a reply. If the model calls a tool, the transition evaluates to `"tool-call"`.

**Key fields:**

| Field | Required | Description |
|-------|----------|-------------|
| `system_instruction` | No | System prompt (supports macros) |
| `execute_config.model` | Yes | Model name, e.g. `qwen2.5:7b` |
| `execute_config.provider` | Yes | `ollama`, `openai`, `vllm`, `gemini` |
| `execute_config.hooks` | No | List of hook names to expose as tools |
| `execute_config.hide_tools` | No | Tools to suppress from the model |
| `execute_config.temperature` | No | Sampling temperature (0–1) |

**Transition values:**
- `"tool-call"` — model issued one or more tool calls
- `"stop"` — model replied with text and stopped
- `"length"` — reply was truncated at token limit

**Example:**
```json
{
  "id": "chat",
  "handler": "chat_completion",
  "system_instruction": "You are a helpful assistant. Today is <span v-pre>{{now:2006-01-02}}</span>.",
  "execute_config": {
    "model": "<span v-pre>{{var:model}}</span>",
    "provider": "<span v-pre>{{var:provider}}</span>",
    "hooks": ["nws"]
  },
  "transition": {
    "branches": [
      { "operator": "equals", "when": "tool-call", "goto": "run_tools" },
      { "operator": "default", "when": "", "goto": "end" }
    ]
  }
}
```

---

## `execute_tool_calls`

Executes the tool calls emitted by the previous `chat_completion` task, appends the results to the chat history, and loops back.

**Key fields:**

| Field | Required | Description |
|-------|----------|-------------|
| `input_var` | Yes | ID of the `chat_completion` task whose output to use |

**Example:**
```json
{
  "id": "run_tools",
  "handler": "execute_tool_calls",
  "input_var": "chat",
  "transition": {
    "branches": [
      { "operator": "default", "when": "", "goto": "chat" }
    ]
  }
}
```

---

## `hook`

Calls a specific tool on a named hook directly — no LLM involved. Use for deterministic side effects (e.g. writing a file, calling a fixed API endpoint).

**Key fields:**

| Field | Required | Description |
|-------|----------|-------------|
| `hook.name` | Yes | Registered hook name (e.g. `local_shell`) |
| `hook.tool_name` | Yes | Tool/operation to call on that hook |
| `hook.args` | No | Static string arguments |
| `output_template` | No | Go template to format the hook output |

**Example:**
```json
{
  "id": "write_file",
  "handler": "hook",
  "hook": {
    "name": "local_fs",
    "tool_name": "write_file",
    "args": { "path": "/tmp/output.txt" }
  }
}
```

---

## `condition_key`

Routes based on a keyword in the model's output. Useful for yes/no decisions.

**Key fields:**

| Field | Required | Description |
|-------|----------|-------------|
| `system_instruction` | Yes | Prompt that asks the model to reply with a keyword |
| `valid_conditions` | Yes | Map of accepted keyword → boolean |

**Example:**
```json
{
  "id": "validate",
  "handler": "condition_key",
  "system_instruction": "Does this look like valid JSON? Reply only 'valid' or 'invalid'.",
  "valid_conditions": { "valid": true, "invalid": true },
  "transition": {
    "branches": [
      { "operator": "equals", "when": "valid",   "goto": "process" },
      { "operator": "equals", "when": "invalid",  "goto": "error_handler" }
    ]
  }
}
```

---

## `prompt_to_string`

Renders a Go template string using accumulated task output variables. Useful for building prompts that combine outputs from multiple previous tasks.

**Key fields:**

| Field | Required | Description |
|-------|----------|-------------|
| `prompt_template` | Yes | Go template string, variables via <span v-pre>`{{.task_id}}`</span> |

---

## `noop`

Passes input through to the next task unchanged. Useful as an explicit routing node.
