---
title: Core Concepts
description: How chains, tasks, tools, transitions, and macros fit together in Contenox.
---

# Core Concepts

## Task Chains

A **chain** is a JSON file that defines how the AI agent behaves — which model to use, what it can do, and how it moves between steps.

Chains are the central building block. The `contenox` CLI and headless runs all use the same chain engine.

Chains aren't limited to AI loops. A single chain can mix LLM steps, direct tool/tools calls, and manual transitions — in any order. Swapping chains is easy:

```bash
# run subcommand — use any chain for this invocation:
contenox run --chain ./my-chain.json "input"
# (falls back to chain-agent-run.json if --chain is omitted)

# chat — set the default session chain:
contenox config set default-chain ./my-chain.json
# (falls back to chain-agent-contenox.json if not set)
```

Fallback chain files are resolved by name, workspace-first: the workspace `.contenox/` copy wins when present, otherwise `~/.contenox/`. The shipped files follow the `chain-<role>-<variant>.json` convention — see [Chain files: naming, roles, and resolution](/docs/guide/chain-naming/) for every role and the full resolution story.

```json
{
  "id": "my-chain",
  "tasks": [ ... ],
  "token_limit": 8192
}
```

## Tasks

Each item in `tasks[]` is a **task** — a single step with a handler, optional LLM config, and a transition rule.

```json
{
  "id": "ask_model",
  "handler": "chat_completion",
  "system_instruction": "You are a helpful assistant.",
  "execute_config": {
    "model": "qwen3:8b",
    "provider": "ollama"
  },
  "transition": {
    "branches": [
      { "operator": "default", "when": "", "goto": "end" }
    ]
  }
}
```

The `handler` determines what the task does. See [Handlers](/docs/specification/handlers) for all types.

## Tools

A **tool** is a capability the model can call — a local shell command, the local filesystem, or a remote HTTP service.

- **`local_shell`** — run shell commands (`contenox run` and `contenox chat` require `--shell`; editor clients route shell execution through their approval surface where supported)
- **`local_fs`** — read/write local files
- **`workspace`** — ask the workspace's semantic index questions via `workspace_search`; see [Workspace index & search](/docs/guide/search/)
- **Remote tools** — any service exposing an OpenAPI v3 spec; by default discovered at `<url>/openapi.json`, overridable with `--spec` at registration time
- **MCP servers** — any Model Context Protocol server (added via `contenox mcp add`)

Tools are listed by name in `execute_config.tools`. Use `["*"]` to expose all registered tools, or list them explicitly for least-privilege access:

```json
"execute_config": {
  "tools": ["nws", "local_shell"]
}
```

> **Important:**
> `"tools": ["*"]` grants the model access to every registered tool in this run.
> For production or sensitive environments, list only the tools the task actually needs.
> This is the per-invocation tool allowlist — the model can only see and call what you explicitly grant.
> What happens when a call is actually made (allow, approve, or deny) is a separate layer: the [HITL policy](/docs/guide/hitl/).
> See [Tools reference](/docs/integrations/tools/) for access control patterns.

Chains are started by you — a prompt, `contenox run`, a fired mission — or, with the opt-in, by the runtime's own internal events: [Events & triggers (beta)](/docs/guide/events/) fires a chain when a matching event lands in the durable log.

## Transitions

After a task runs, the chain evaluates **transition branches** to decide the next task.

```json
"transition": {
  "branches": [
    { "operator": "equals", "when": "tool_call", "goto": "run_tools" },
    { "operator": "default", "when": "",          "goto": "end" }
  ]
}
```

Branches are evaluated top to bottom. `"goto": "end"` terminates the chain.

The same branches are what build the agentic loop — a `tool_call` branch into an execute step and a back-edge — walked through in [The agentic loop](/docs/guide/agentic-loop/). Branches on a `route` task do something else: they pick which loop runs at all, which is how one chain serves requests that need different methods and different tools. See [Request routing](/docs/guide/request-routing/).

## Data flow

Output from each task is passed as input to the next. Use `input_var` to read from a specific previous task instead of the immediately preceding one:

```json
{
  "id": "run_tools",
  "handler": "execute_tool_calls",
  "input_var": "ask_model"
}
```

## Macros

Chain JSON supports runtime macros inside string fields:

| Macro | Expands to |
|-------|-----------|
| `{{var:model}}` | The active model name from config |
| `{{var:provider}}` | The active provider from config |
| `{{var:alt_model}}` | Optional secondary model from config |
| `{{var:alt_provider}}` | Optional secondary provider from config |
| `{{var:max_tokens}}` | Optional response token cap from config or `--max-tokens` |
| `{{now:2006-01-02}}` | Current date (Go time format) |
| `{{toolservice:list}}` | JSON manifest of tools visible to the current task |

### Fallbacks

A `{{var:…}}` macro can supply a fallback for when the variable is missing or empty:

| Form | Expands to |
|------|-----------|
| `{{var:name\|literal}}` | the variable's value, or the literal text `literal` when it is unset/empty |
| `{{var:name\|var:other}}` | the variable's value, or another variable `other` when the first is unset/empty |

For example, `{{var:alt_model\|var:model}}` falls back to the primary chat model when no alt model is configured.

See [Transitions & Branching](/docs/specification/transitions) and [Handlers](/docs/specification/handlers) for the full reference.

The chain is also the first of two artifacts: it says what happens, and the [HITL policy](/docs/guide/hitl/) says what is permitted. Why that split is the structural difference — and what contenox shares with the dedicated coding agents — is in [How contenox compares](/docs/guide/comparison/).
