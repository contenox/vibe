# Core Concepts

## Task Chains

A **chain** is a JSON file that defines how the AI agent behaves — which model to use, what it can do, and how it moves between steps.

Chains are the central building block. `vibe`, the runtime API, and the EE all run the same chain engine.

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
    "model": "qwen2.5:7b",
    "provider": "ollama"
  },
  "transition": {
    "branches": [
      { "operator": "default", "when": "", "goto": "end" }
    ]
  }
}
```

The `handler` determines what the task does. See [Handlers](/chains/handlers) for all types.

## Hooks

A **hook** is a capability the model can call — a local shell command, the local filesystem, or a remote HTTP service.

- **`local_shell`** — run shell commands (requires `--enable-local-exec`)
- **`local_fs`** — read/write files
- **Remote hooks** — any service with an OpenAPI v3 spec at `/openapi.json`

Hooks are listed by name in `execute_config.hooks`. The engine fetches the remote hook's schema and gives every operation to the model as a callable tool.

## Transitions

After a task runs, the chain evaluates **transition branches** to decide the next task.

```json
"transition": {
  "branches": [
    { "operator": "equals", "when": "tool-call", "goto": "run_tools" },
    { "operator": "default", "when": "",          "goto": "end" }
  ]
}
```

Branches are evaluated top to bottom. `"goto": "end"` terminates the chain.

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
| <span v-pre>`{{var:model}}`</span> | The active model from config |
| <span v-pre>`{{var:provider}}`</span> | The active provider from config |
| <span v-pre>`{{now:2006-01-02}}`</span> | Current date (Go time format) |
| <span v-pre>`{{hookservice:list}}`</span> | Comma-separated list of registered hook names |

See [Transitions & Branching](/chains/transitions) and [Handlers](/chains/handlers) for the full reference.
