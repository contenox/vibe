---
title: Task Chains
description: The JSON state machine that defines how your agent behaves — composable, inspectable, backend-agnostic.
---

# Task Chains

A task chain is a JSON state machine that defines how the AI agent behaves end-to-end. Chains are composable, inspectable, and backend-agnostic.

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
| `token_limit` | int | Max token budget for the chat history |
| `debug` | bool | Enable verbose task-level logging |

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
