# Handlers

Every task has a `handler` field that determines what it does. This page documents all available handlers and which fields are valid for each.

## Handler types

| Handler | What it does |
|---------|-------------|
| `chat_completion` | Send messages to an LLM, receive a text/tool-call reply |
| `execute_tool_calls` | Execute the tool calls from the previous LLM reply |
| `tools` | Call a specific named tools tool directly (no LLM involved) |
| `route` | LLM picks exactly one of the declared branch labels; routing-only, input passes through unchanged |
| `raise_error` | Immediately halt the chain with an error message |
| `noop` | Pass input through unchanged |

---

## `chat_completion`

Sends the current input to the LLM and waits for a reply. If the model calls a tool, the transition evaluates to `"tool_call"`.

**Key fields:**

| Field | Required | Description |
|-------|----------|-------------|
| `system_instruction` | No | System prompt (supports macros) |
| `execute_config.model` | Yes | Model name, e.g. `qwen2.5:7b` |
| `execute_config.provider` | Yes | `ollama`, `openai`, `anthropic`, `vllm`, `gemini`, `bedrock`, `vertex-google` |
| `execute_config.tools` | No | Tools allowlist: `[]`=none, `["*"]`=all, `["a","b"]`=named, `["*","!x"]`=all-except. Absent/`null`=none — the task has no tools until this field explicitly grants some. |
| `execute_config.hide_tools` | No | Tools to suppress (by namespaced name) from **both** the registry tools selected via `tools` and any client-passed tools |
| `execute_config.tools_policies` | No | Per-tools-provider policy overrides, `{ "<tools_name>": { "<key>": "<value>" } }`. Injected before the tool runs, so the provider can enforce them (e.g. `local_shell: { "_allowed_commands": "git,go,ls", "_denied_commands": "sudo,rm" }`). |
| `execute_config.pass_clients_tools` | No | Boolean. When true, tools supplied by the calling client (e.g. an ACP editor) are exposed to the model for this task, in addition to the registry `tools` allowlist. Default false. |
| `execute_config.temperature` | No | Sampling temperature (0–1) |
| `execute_config.think` | No | Reasoning effort level. One of `auto`, `off`, `minimal`, `low`, `medium`, `high`, `xhigh` (plus boolean-style aliases like `"true"`/`"false"`). Empty = provider default. Supported by Ollama (v0.17.5+), Gemini 2.5+, vLLM, and OpenAI o-series models. |
| `execute_config.max_tokens` | No | Cap on the model's output tokens for this task. When unset, **no** explicit output cap is sent and the provider default applies — the engine deliberately does **not** fall back to the chain's `token_limit` (that is the input+output context window, not an output cap, and conflating them trips per-model output limits, e.g. Vertex Gemini 2.5 Pro's 65536 cap). |
| `execute_config.shift` | No | Boolean. If true, slides the context window by dropping old messages instead of erroring on token limits. |
| `execute_config.models` | No | Array of fallback model IDs tried in order when the primary model is unavailable. |
| `execute_config.providers` | No | Array of fallback provider types, paired index-for-index with `models`. |

| `execute_config.retry_policy` | No | LLM-call retry and model-fallback settings — see [`retry_policy`](#retry_policy) below. |

**Transition values:**
- `"tool_call"` — model issued one or more tool calls
- `"executed"` — model replied with text and finished (no tool calls)

(These are control tokens, not the model's text. To branch on the model's actual
answer, use the [`route`](#route) handler.)

### Image input

Chat-history messages can carry image attachments beside their text `content`:

```json
{ "role": "user", "content": "What is in this screenshot?",
  "images": [ { "data": "<base64>", "mime_type": "image/png" } ] }
```

A request containing images resolves only to models that declare the vision
capability. When no matching vision-capable model is available the task fails
with a distinct error — images are never silently dropped to fall back on a
text-only model. The served OpenAI-compatible `chat/completions` endpoint also
accepts the standard content-parts array form
(`{"type":"image_url","image_url":{"url":"data:image/png;base64,..."}}`) and
decodes it into these attachments; only inline `data:` URIs are accepted — the
runtime never fetches remote image URLs on a client's behalf.

### `retry_policy`

Controls automatic retries on transient LLM errors and optional model swapping after repeated failures.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `max_attempts` | int | `1` | Total attempts including the first (`0` or `1` disables retry) |
| `initial_backoff` | duration | `"500ms"` | Wait before the second attempt; doubled each retry |
| `max_backoff` | duration | — | Cap on exponential backoff |
| `jitter` | float | `0` | 0–1 fraction of backoff added as random noise |
| `rate_limit_min_wait` | duration | — | Minimum wait when the provider returns a rate-limit error |
| `fallback_model_id` | string | — | Alternate model ID to switch to after `fallback_after` consecutive failures |
| `fallback_after` | int | — | Failure count that triggers the model swap |

**Example:**
```json
{
  "id": "chat",
  "handler": "chat_completion",
  "system_instruction": "You are a helpful assistant. Today is {{now:2006-01-02}}.",
  "execute_config": {
    "model": "{{var:model}}",
    "provider": "{{var:provider}}",
    "tools": ["nws"]
  },
  "transition": {
    "branches": [
      { "operator": "equals", "when": "tool_call", "goto": "run_tools" },
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
| `input_var` | No | ID of the `chat_completion` task whose output to execute. Optional — when omitted, the immediately preceding task's output is used. Set it explicitly to run tool calls from a specific earlier task. |

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

## `tools`

Calls a specific tool on a named tool directly — no LLM involved. Use for deterministic side effects (e.g. writing a file, calling a fixed API endpoint).

**Key fields:**

| Field | Required | Description |
|-------|----------|-------------|
| `tools.name` | Yes | Registered tools-provider name (the service/server, e.g. `local_fs`, `slack`). A `tools` task with no `tools` block errors. |
| `tools.tool_name` | No | The specific tool/operation to invoke on that provider (e.g. `write_file`). Not validated at chain-load time; set it in practice (an unset/unknown value surfaces as a call-time error), but it may be omitted for a provider whose sole tool matches the provider name. |
| `tools.args` | No | Static arguments passed to the tool |
| `output_template` | No | Go `text/template` string rendered against the tools's JSON response. Variables are the response fields (e.g. `{{.exit_code}}`). Output stored as a string. |

**Example:**
```json
{
  "id": "write_file",
  "handler": "tools",
  "tools": {
    "name": "local_fs",
    "tool_name": "write_file",
    "args": { "path": "/tmp/output.txt" }
  }
}
```

---

## `route`

Asks the LLM to classify the input into exactly one of the labels declared by this task's own `equals` transition branches, then routes to the matching task. The routes are the branch `when` values — there is no separate schema; the chain you can read *is* the route set.

`route` is **routing-only**: the task's input passes through to the next task unchanged. It produces a control-flow decision, never transformed data — so a router can never silently reshape what a downstream task sees.

The model's answer is normalized before routing: the engine first looks for a **case-insensitive exact** match against a declared label, then a **case-insensitive substring** match (a label contained anywhere in the answer). Only if neither matches is the `default` branch taken. This substring fallback means a model that replies `"I think this is urgent"` still routes to the `urgent` label — but it also means overlapping labels (e.g. `normal` vs `abnormal`) can collide, so keep route labels distinct.

**Key fields:**

| Field | Required | Description |
|-------|----------|-------------|
| `system_instruction` | No | Describes the classification; the engine appends the allowed labels |
| `execute_config.model` / `provider` | Yes | Model to use |
| `transition.branches` | Yes | The `equals` branches whose `when` values are the route labels; include a `default` |

---

## Common task fields

These fields are valid on **any** task, regardless of handler:

| Field | Description |
|-------|-------------|
| `input_var` | Read this task's input from the named earlier task's output instead of the immediately preceding task. |
| `prompt_template` | Go `text/template` text sent to the LLM as the prompt. When set it overrides the resolved input as the prompt. Supports variables from previous task outputs (e.g. `{{.input}}`, `{{.some_task_id}}`). |
| `print` | Go `text/template` string formatted and emitted as a print event (display/logging) when the task completes — it does **not** change the task's output. Supports the same template variables (e.g. `"Validation result: {{.validate_input}}"`). |
| `input_max_bytes` | Caps oversized string / chat-history input before this task runs. Intended for recovery or summarization tasks that should explain a failure without re-feeding the same huge input that caused it. |
| `timeout` | Per-task execution timeout, e.g. `"30s"`, `"2m"`, `"1h"`. |
| `retry_on_failure` | Integer count of times to retry this task on failure (default `0`). Applies to all handlers, including `tools`. |

## Template functions

The Go `text/template` fields — `prompt_template` and `output_template` — are rendered with the [Sprig](https://masterminds.github.io/sprig/) function library available in addition to the built-ins. This is what turns a structured task output into clean prompt input instead of Go's default struct formatting:

```text
{{ .scan_result | toJson }}            serialize a JSON/struct value as clean JSON
{{ .page_text | trunc 4000 }}            cap an oversized tool output
{{ (last .history.Messages).Content }}   pull a single field out
```

Without it, injecting a non-string task output (a tool's JSON result, a chat history) into a template renders Go syntax like `map[...]` or `{[{...}]}` that the model cannot reliably parse — which is why transforming or evaluating tool output used to mean shelling out. `toJson`, `trunc`, and the rest of Sprig remove that step.

---

## `noop`

Passes input through to the next task unchanged. Useful as an explicit routing node.

---

## `raise_error`

Immediately halts the chain with the input string as the error message. Use as a terminal error branch — no transition needed.

