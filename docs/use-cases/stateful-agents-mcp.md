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

Grant one to an agent with a single line in its declaration's front matter —
`mcpServers: [memory]` in `.contenox/agents/<name>.md`, and nothing else; see
[declaring agents](/docs/guide/agents/). A chain that needs the same grant sets
`"tools": ["memory"]` (or `["filesystem"]`, `["fetch"]`) in its
`execute_config` instead — see [Request routing](/docs/guide/chains/routing/)
for how a chain's tasks reach tools.

## What each server is for

**Filesystem** — the model reads real files from disk. Point an agent at a
directory and ask for a summary, an inventory, or a report built from what's
actually there.

**Memory** — a knowledge graph that outlives any single run. `stdio` MCP
servers are spawned as child processes and terminated on exit, but
`@modelcontextprotocol/server-memory` writes its graph to disk, so a fact
stored in one session is still there in a completely separate one. Persistence
here is a property of the server you chose, not of contenox.

**Fetch** — live web pages, brought into context on request. Give it a URL
and ask for a summary; the model calls the server's fetch tool and reasons
over the current page content.
