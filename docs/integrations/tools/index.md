---
description: How contenox turns files, commands, and APIs into schemas a model can call — and how the allowlist decides what's on the table.
order: 2
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

An [agent declaration](/docs/guide/agents/) names its tools on one line — `tools: Read, Glob, Grep` — and omitting the line inherits every tool. That is where most tool scoping happens.

In the chain behind it, and in a chain you author yourself, the same allowlist is `execute_config.tools`, per task:

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
| `["*"]` | **Every connected toolset, with no exceptions** |
| `["a", "b"]` | Only the named toolsets |
| `["*", "!local_shell"]` | All except `local_shell` |

Unknown names in an exact list are silently ignored — if `local_shell` is disabled the chain still runs.

`"*"` admits everything this machine has connected: the toolsets contenox hosts, the `native-` in-process ones, every MCP server and OpenAPI service you registered, and the `decl-` sources an [agent declaration](/docs/guide/agents/#tools-an-agent-brings-with-it) brought with it. Those prefixes are **namespaces** — they stop a declared source from colliding with an in-process toolset — and never a hidden exclusion. To leave one out, say so: `"!native-git"` removes it, and an exclusion wins over `"*"` wherever the two appear in the list.

Use `{{tools}}` in your `system_instruction` to inject the live tool manifest. It respects the task's `tools` allowlist — the model only sees what the task permits:

```json
"system_instruction": "You are a helpful assistant. Available tools: {{tools}}."
```

Nothing is added to a `system_instruction` that it does not declare.

## Template variables

System instructions and `prompt_template` fields support the following macros:

| Macro | Returns |
|-------|---------|
| `{{var:<name>}}` | Value of the named template variable supplied by the caller |
| `{{now}}` | Current time in RFC3339 format |
| `{{now:<layout>}}` | Current time in Go time layout (e.g. `{{now:2006-01-02}}`) |
| `{{chain:id}}` | ID of the currently executing chain |
| `{{tools}}` | JSON object mapping toolset name → array of tool names (respects task `tools` allowlist) |
| `{{host:os}}` | Host operating system (`linux`, `darwin`, `windows`) |
| `{{host:arch}}` | Host architecture (`amd64`, `arm64`) |
| `{{toolservice:list}}` | Same as `{{tools}}` |
| `{{toolservice:tools}}` | JSON array of tools names available to the task |
| `{{toolservice:tools <name>}}` | JSON array of tool names for a specific tool |

## Tools types

Contenox ships with built-in local tools and supports unlimited remote tools:

| Tools name | Type | Always available | What it does |
|---|---|---|---|
| `local_fs` | Local | Via ACP client | Read, write, and edit a file you already know the path to — five tools (`read_file`, `write_file`, `edit_file`, `sed`, `read_file_range`), read-before-write contract for mutations. Forwarded to the ACP client's `fs/*` capability. |
| `local_shell` | Local | Via ACP client | Run shell commands — including listing, searching, and globbing (`ls`, `find`, `grep`/`rg`) — forwarded to the ACP client's `terminal/*` capability, governed by HITL policy. |
| MCP servers | Remote | Operator attaches | Any MCP-compatible server (filesystem, memory, SaaS, internal tools) — see [Model Context Protocol](/docs/integrations/tools/mcp/) |
| _your name_ | Remote | Register with `contenox tools add` | Any OpenAPI v3 service |

## Choosing the right tools

- **`local_fs`** — best for reading, writing, and editing a file whose path you already know. The read-before-write contract and sandbox guard against confabulated edits.
- **`local_shell`** — for everything else on the machine: listing directories, searching/grepping, globbing, and running build or test commands. Use only in trusted, sandboxed environments.
- **MCP servers** — attach any MCP-compatible server for retrieval, memory, or a SaaS integration; see [Model Context Protocol](/docs/integrations/tools/mcp/).
- **Remote tools** — turn any OpenAPI service into an agent tool; ideal for internal APIs and team-shared tools without an MCP server.

## Further reading

- [Remote Tools](/docs/integrations/tools/remote/) — register external APIs as agent tools
- [Local Tools](/docs/integrations/tools/local/) — built-in in-process tools reference
