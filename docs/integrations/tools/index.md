---
description: How contenox turns files, commands, and APIs into schemas a model can call — and how the allowlist decides what's on the table.
---

# What are Tools?

Tools are the mechanism by which Contenox gives a model access to real-world actions. Instead of generating text, the model calls a tools to read files, run commands, query APIs, or fire HTTP requests — and gets the result back as context for its next reply.

## How it works

```
Chain starts
  └─ FetchTools: each listed tools returns its tool schemas
       └─ Schemas are sent to the model alongside the prompt
            └─ Model returns a tool call
                 └─ execute_tool_calls runs the tools
                      └─ Result appended to history → model continues
```

In your chain JSON, specify which tools the task can use via the `execute_config.tools` allowlist:

```json
"execute_config": {
  "model": "qwen3:8b",
  "provider": "ollama",
  "tools": ["local_fs", "nws", "local_shell"]
}
```

Pattern support:

| Value | Meaning |
|---|---|
| field absent / `null` | No registered tools exposed to the model |
| `[]` | No tools exposed to the model |
| `["*"]` | All registered tools |
| `["a", "b"]` | Only the named tools |
| `["*", "!local_shell"]` | All except `local_shell` |

Unknown names in an exact list are silently ignored — if `local_shell` is disabled the chain still runs.

Use `{{toolservice:list}}` in your `system_instruction` to inject the live tool manifest. This macro respects the task's `tools` allowlist — the model only sees what the task permits:

```json
"system_instruction": "You are a helpful assistant. Available tools: {{toolservice:list}}."
```

## Template variables

System instructions and `prompt_template` fields support the following macros:

| Macro | Returns |
|-------|---------|
| `{{var:<name>}}` | Value of the named template variable supplied by the caller |
| `{{now}}` | Current time in RFC3339 format |
| `{{now:<layout>}}` | Current time in Go time layout (e.g. `{{now:2006-01-02}}`) |
| `{{chain:id}}` | ID of the currently executing chain |
| `{{toolservice:list}}` | JSON object mapping tools name → array of tool names (respects task `tools` allowlist) |
| `{{toolservice:tools}}` | JSON array of tools names available to the task |
| `{{toolservice:tools <name>}}` | JSON array of tool names for a specific tool |

## Tools types

Contenox ships with built-in local tools and supports unlimited remote tools:

| Tools name | Type | Always available | What it does |
|---|---|---|---|
| `local_fs` | Local | ✅ | Read, write, and search files within a configured directory (10 verb-specific tools, read-before-write contract for mutations) |
| `git` | Local | `contenox chat`/`run`/`new`, ACP sessions | Read and mutate the workspace's own Git repository in-process — status, diff, log, show, branches, blame, add, commit, checkout, restore |
| `gointel` | Local | `contenox chat`/`run`/`new`, ACP sessions | Six read-only Go code-intelligence tools (`go_describe`, `go_definition`, `go_references`, `go_implementations`, `go_symbols`, `go_diagnostics`) backed by a real type checker |
| `jq` | Local | `contenox chat`/`run`/`new`, ACP sessions | `jq_query` — run a jq program over a JSON or YAML document, read-only |
| `workspace` | Local | `contenox chat`/`run`/`new`, ACP sessions | `workspace_search` — semantic search over the index built by `contenox index` |
| `goja` | Local | `contenox chat`/`run`/`new`, ACP sessions — **beta**, requires `opt-in-beta` | `goja_eval` — a JavaScript (ES2023) sandbox with no ambient I/O, plus one tool per operator-authored script under `$CONTENOX_DIR/tools/*.js` |
| `webtools` | Local | ✅ | Call HTTP endpoints — `web_get`, `web_head`, `web_post`, `web_put`, `web_patch`, `web_delete`. SSRF guarding is opt-in (`_denied_hosts` is empty by default — see [local tools](/docs/integrations/tools/local/)); mutating verbs HITL-approve by default. |
| `local_shell` | Local | CLI opt-in | Run shell commands. `contenox run` and `contenox chat` require `--shell`; editor clients route shell execution through their own approval surface where supported. |
| `print` | Local | ✅ | Append a message to the chat history or return it as a string |
| `echo` | Local | ✅ | Echo the input back (useful for debugging chains) |
| _your name_ | Remote | Register with `contenox tools add` | Any OpenAPI v3 service |

## Choosing the right tools

- **`local_fs`** — best for code analysis, file editing, report generation. Prefer over `local_shell` for file ops; the read-before-write contract and sandbox guard against confabulated edits.
- **`git`** — prefer over `local_shell` for repository operations (status, diff, log, commit, branch, restore); each operation is a separate tool the HITL policy can gate individually, rather than one decision for all of `git`.
- **`gointel`** — exact questions about Go code (type, signature, references, implementers). Prefer over `grep`/`local_shell` — it answers from a type checker, not text search.
- **`jq`** — pull one field or projection out of a JSON/YAML file instead of reading the whole thing.
- **`workspace`** — semantic, meaning-based search over an already-built `contenox index`; approximately right, not exact — use `gointel` when you need an exact Go-symbol answer.
- **`goja`** — imperative logic or multi-tool composition over data you already have, via `host.tool(...)`; reach for `jq` instead for simple declarative shape-work. Beta: requires `contenox config set opt-in-beta true` (or `CONTENOX_OPT_IN_BETA=1`); absent otherwise.
- **`webtools`** — when the model needs to call HTTP. Use `web_get` / `web_head` for retrieval; mutating verbs trigger HITL approval by default.
- **`local_shell`** — full power; use only in trusted, sandboxed environments. Reach for it for build / test scripts, not for cat / grep / sed / git against project files — those have dedicated tools.
- **`print`** / **`echo`** — inject messages or inspect task output during development
- **Remote tools** — turn any OpenAPI service into an agent tool; ideal for internal APIs, SaaS integrations, and team-shared tools

## Further reading

- [Remote Tools](/docs/integrations/tools/remote/) — register external APIs as agent tools
- [Local Tools](/docs/integrations/tools/local/) — built-in in-process tools reference
