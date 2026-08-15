---
title: The chain format
description: The reference for the JSON state machine behind an agent — the artifact you read to see what it may do, and the one you write when you need a guarantee instead of an instruction.
---

# The chain format

This is a reference for a file you mostly do not write. An agent is
[a Markdown declaration](/docs/guide/agents/); contenox generates the chain
behind it and keeps it in step with the file you edited.

You come here for two reasons. To **read** one — the chain is the audit
artifact, the single place that says which model runs, which tools are on the
table, and where the run can go next. And to **write** one, when a declaration
cannot say what you need it to: a branch, a different model per step, a
recovery path, a declared point where a human stands. Then the engine runs
exactly what you wrote.

![Task Chain Execution Flow](/chain_flow_diagram.png)

## Chain structure

```json
{
  "id": "my-chain",
  "description": "What this chain does",
  "tasks": [ /* TaskDefinition[] */ ],
  "token_limit": 8192,
  "debug": false
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique identifier |
| `description` | string | Human-readable description |
| `tasks` | TaskDefinition[] | Ordered list of task definitions |
| `token_limit` | int | Context budget for the chat history, and the source of the per-call tool-result cap. **Set it.** See below. |
| `debug` | bool | Enable verbose task-level logging |

> **`token_limit` also sizes tool results.** The largest result a tool may
> return is derived from what is left of this budget after the chat history.
> Omitting the field does not mean "no limit" — it leaves the budget at zero, so
> every tool call comes back as `tool_result_too_large` no matter how small the
> real result is. A chain whose tools all report that error is missing its
> `token_limit`.

## Task structure

```json
{
  "id": "step_name",
  "description": "What this task does",
  "handler": "chat_completion",
  "system_instruction": "...",
  "execute_config": { },
  "transition": { "branches": [ ] },
  "retry_on_failure": 0,
  "timeout": "30s"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique identifier for this task |
| `description` | string | Human-readable summary of what the task does |
| `handler` | string | Handler type — see [Handlers](/docs/specification/handlers) |
| `system_instruction` | string | System prompt (supports template macros) |
| `execute_config` | object | Model, provider, tools, and execution policy settings |
| `transition` | object | Branching rules — see [Transitions](/docs/specification/transitions) |
| `retry_on_failure` | int | Number of times to retry if the task errors (default: `0`) |
| `timeout` | string | Per-task timeout, e.g. `"30s"` or `"2m"` |

See [Handlers](/docs/specification/handlers) and [Transitions](/docs/specification/transitions) for the full field reference.

## Sections

- **[Handlers](/docs/specification/handlers)** — all task handler types and their fields
- **[Transitions & Branching](/docs/specification/transitions)** — how the chain decides what to do next
- **[Annotated Examples](/docs/specification/examples)** — full working chains with commentary
- **[Writing a chain by hand](/docs/guide/first-chain/)** — the walkthrough, when you have decided you need one
