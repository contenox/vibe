---
title: Core Concepts
description: How agents, chains, tasks, tools, transitions, and macros fit together in Contenox.
---

# Core Concepts

## Agents

An **agent** is a Markdown file with a YAML frontmatter header: the frontmatter says how to run it, the body becomes its system prompt.

```markdown
---
name: reviewer
description: Reviews a file for correctness problems
tools: Read, Glob, Grep
---

You are a code reviewer.
```

Drop it in `.contenox/agents/` and the next run picks it up. What a declaration cannot say — context budgets, retries, loop bounds, shell allowlists — lives in [`agents.toml`](/docs/reference/agents-config/) beside it. Between them, that is the whole authoring surface for most agents. See [Declaring agents](/docs/guide/agents/).

## Task Chains

Behind each declaration contenox builds a **chain**: a JSON state machine that says which model to use, what the agent can do, and how it moves between steps. You do not maintain it — edit the declaration and it follows.

You can also write one by hand, and that is the reason the format is documented rather than hidden. A declaration is one prompt, one tool list, one permission setting: it cannot branch, cannot vary the model per step, cannot declare a recovery path or a point where a human is required. When you need one of those, you write the chain.

Chains aren't limited to AI loops. A single chain can mix LLM steps, direct tool/tools calls, and manual transitions — in any order:

```bash
# fire any registered chain as a mission:
contenox mission fire my-chain "input" --wait

# or set it as a session's default chain:
contenox config set default-chain ./my-chain.json
```

Fallback chain files are resolved by name: the workspace `.contenox/` copy wins when present, then `~/.contenox/`, then the shipped copies in `~/.contenox/system/`. The shipped files follow the `chain-<role>-<variant>.json` convention — see [Chain files: naming, roles, and resolution](/docs/guide/chain-naming/) for every role and the full resolution story.

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

- **`local_shell`** — run shell commands, forwarded to the ACP client's `terminal/*` capability and governed by HITL policy
- **`local_fs`** — read, write, and edit local files (`read_file`, `write_file`, `edit_file`, `sed`, `read_file_range`), forwarded to the ACP client's `fs/*` capability
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

Chains are started by you — a session prompt, a fired mission — or, with the opt-in, by the runtime's own internal events: [Events & triggers (beta)](/docs/guide/events/) fires a chain when a matching event lands in the durable log.

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
| `{{tools}}` | JSON object mapping toolset name → its tool names, filtered to the task's `tools` allowlist |
| `{{host:os}}` | Host operating system (`linux`, `darwin`, `windows`) |
| `{{host:arch}}` | Host architecture (`amd64`, `arm64`) |
| `{{toolservice:list}}` | JSON manifest of tools visible to the current task |

A `system_instruction` is sent as written. Macros are the only substitution — a
task that does not declare `{{tools}}` does not receive a tool manifest.

### Fallbacks

A `{{var:…}}` macro can supply a fallback for when the variable is missing or empty:

| Form | Expands to |
|------|-----------|
| `{{var:name\|literal}}` | the variable's value, or the literal text `literal` when it is unset/empty |
| `{{var:name\|var:other}}` | the variable's value, or another variable `other` when the first is unset/empty |

For example, `{{var:alt_model\|var:model}}` falls back to the primary chat model when no alt model is configured.

See [Transitions & Branching](/docs/specification/transitions) and [Handlers](/docs/specification/handlers) for the full reference.

The chain is also the first of two artifacts: it says what happens, and the [HITL policy](/docs/guide/hitl/) says what is permitted. Why that split is the structural difference — and what contenox shares with the dedicated coding agents — is in [How contenox compares](/docs/guide/comparison/).
