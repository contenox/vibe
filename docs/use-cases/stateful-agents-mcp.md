---
description: Give a model a persistent memory graph, the local filesystem, and live web pages through native MCP — state that survives across calls.
---

# Stateful Agents with MCP

Connect your models to the local filesystem, a persistent memory graph, and live web pages using Contenox's native MCP (Model Context Protocol) integration.

## Prerequisites

Run these commands once to register the three built-in local MCP servers:

```bash
# Register local MCP servers (one-time setup)
contenox mcp add filesystem --transport stdio \
  --command npx --args "-y,@modelcontextprotocol/server-filesystem,$PWD"

contenox mcp add memory --transport stdio \
  --command npx --args "-y,@modelcontextprotocol/server-memory"

contenox mcp add fetch --transport stdio \
  --command npx --args "-y,fetch-mcp"
```

Save example chains once in your project:

```bash
cat > .contenox/chain-mcp-filesystem.json <<'EOF'
{
  "id": "chain-mcp-filesystem",
  "tasks": [{
    "id": "chat",
    "handler": "chat_completion",
    "system_instruction": "You can use the filesystem MCP server when needed. Available tools: {{toolservice:list}}.",
    "execute_config": {
      "model": "{{var:model}}",
      "provider": "{{var:provider}}",
      "tools": ["filesystem"],
      "pass_clients_tools": false
    },
    "transition": {
      "branches": [
        {"operator": "equals", "when": "tool_call", "goto": "run_tools"},
        {"operator": "default", "when": "", "goto": "end"}
      ]
    }
  }, {
    "id": "run_tools",
    "handler": "execute_tool_calls",
    "input_var": "chat",
    "transition": {
      "branches": [{"operator": "default", "when": "", "goto": "chat"}]
    }
  }],
  "token_limit": 131072
}
EOF

cat > .contenox/chain-mcp-memory.json <<'EOF'
{
  "id": "chain-mcp-memory",
  "tasks": [{
    "id": "chat",
    "handler": "chat_completion",
    "system_instruction": "You can use the memory MCP server when needed. Available tools: {{toolservice:list}}.",
    "execute_config": {
      "model": "{{var:model}}",
      "provider": "{{var:provider}}",
      "tools": ["memory"],
      "pass_clients_tools": false
    },
    "transition": {
      "branches": [
        {"operator": "equals", "when": "tool_call", "goto": "run_tools"},
        {"operator": "default", "when": "", "goto": "end"}
      ]
    }
  }, {
    "id": "run_tools",
    "handler": "execute_tool_calls",
    "input_var": "chat",
    "transition": {
      "branches": [{"operator": "default", "when": "", "goto": "chat"}]
    }
  }],
  "token_limit": 131072
}
EOF

cat > .contenox/chain-mcp-fetch.json <<'EOF'
{
  "id": "chain-mcp-fetch",
  "tasks": [{
    "id": "chat",
    "handler": "chat_completion",
    "system_instruction": "You can use the fetch MCP server when needed. Available tools: {{toolservice:list}}.",
    "execute_config": {
      "model": "{{var:model}}",
      "provider": "{{var:provider}}",
      "tools": ["fetch"],
      "pass_clients_tools": false
    },
    "transition": {
      "branches": [
        {"operator": "equals", "when": "tool_call", "goto": "run_tools"},
        {"operator": "default", "when": "", "goto": "end"}
      ]
    }
  }, {
    "id": "run_tools",
    "handler": "execute_tool_calls",
    "input_var": "chat",
    "transition": {
      "branches": [{"operator": "default", "when": "", "goto": "chat"}]
    }
  }],
  "token_limit": 131072
}
EOF
```

## Recipe 1: Filesystem Explorer

Ask the model to read real files from disk and generate a report:

```bash
contenox run \
  --chain .contenox/chain-mcp-filesystem.json \
  --provider openai --model gpt-5-mini \
  "List all JSON files directly inside the current project's .contenox directory (./.contenox only) whose names start with chain-. \
   Read each one and return a markdown table: filename | what the chain does."
```

**Example output:**

| filename                    | what the chain does |
|-----------------------------|---------------------|
| chain-mcp-filesystem.json   | Chat chain with access to the local filesystem via the MCP filesystem server. |
| chain-mcp-memory.json       | Chat chain with access to a persistent key-value memory store via the MCP memory server. |
| chain-mcp-fetch.json        | Chat chain with access to the fetch MCP server for live web content. |

## Recipe 2: Persistent Memory (state across separate invocations)

Store a fact in one run:

```bash
contenox run \
  --chain .contenox/chain-mcp-memory.json \
  --provider openai --model gpt-5-mini \
  "Remember: the project name is Contenox and the version is 0.2.4."
```

Retrieve it in a completely separate run (new process):

```bash
contenox run \
  --chain .contenox/chain-mcp-memory.json \
  --provider openai --model gpt-5-mini \
  "What project and version did I ask you to remember?"
```

The model uses `search_nodes` on the memory graph and replies:  
*"You asked me to remember the project 'Contenox' with version '0.2.4'."*

> [!TIP]
> `contenox run` is intentionally stateless for predictability and scripting safety. `stdio` MCP servers are spawned as child processes and terminated on exit.  
> For cross-invocation persistence, use servers that manage their own storage (e.g. `@modelcontextprotocol/server-memory` writes to disk) or remote HTTP/SSE servers you control.

## Recipe 3: Live Web Research

Fetch and summarize any live page:

```bash
contenox run \
  --chain .contenox/chain-mcp-fetch.json \
  --provider openai --model gpt-5-mini \
  "Use the fetch tool to fetch https://modelcontextprotocol.io and give me a one-paragraph summary."
```

The model calls `fetch_url`, receives the current HTML, and returns a clean summary.

## How the chains work

All three example chains use the same simple structure:

```json
{
  "tasks": [{
    "handler": "chat_completion",
    "system_instruction": "...Available tools: {{toolservice:list}}.",
    "execute_config": {
      "tools": ["filesystem"],
      "pass_clients_tools": false
    }
  }]
}
```

- `tools` is the allowlist of MCP servers the model can access as tools. Use `["*"]` to include all registered servers, or `["*", "!name"]` to exclude one.  
- `{{toolservice:list}}` injects the live tool manifest into the system prompt — filtered to only the tools the task allows.  
- The task engine automatically handles the full tool-call loop — no manual branching required.

> [!TIP]
> Add `--trace` to watch every MCP tool call, its arguments, and results in real time.
