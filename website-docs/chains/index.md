---
title: Task Chains
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
  "handler": "chat_completion",
  "system_instruction": "...",
  "execute_config": { },
  "transition": { "branches": [ ] },
  "retry_on_failure": 0,
  "timeout": "30s"
}
```

See [Handlers](/chains/handlers) and [Transitions](/chains/transitions) for the full field reference.

## Sections

- **[Handlers](/chains/handlers)** — all task handler types and their fields
- **[Transitions & Branching](/chains/transitions)** — how the chain decides what to do next
- **[Annotated Examples](/chains/examples)** — full working chains with commentary
