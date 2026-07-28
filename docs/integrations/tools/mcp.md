---
description: Connect any MCP server — local, SSE, or HTTP — with persistent, session-scoped connections instead of one-shot calls.
---

# Model Context Protocol (MCP)

Contenox is a full native MCP client. Every chat session and `contenox run` invocation can connect to any MCP-compatible server—local child processes, remote SSE streams, or HTTP endpoints.

## What is MCP?

The [Model Context Protocol](https://modelcontextprotocol.io/) is an open standard (originally created by Anthropic and donated to the [AI Agentic Foundation](https://aaif.ai/) at the Linux Foundation) that lets AI agents talk to tools, memory stores, and data sources using a universal wire format.

Think of it as USB-C for AI: one standard connection, unlimited devices.

## What makes Contenox's MCP implementation different

Most clients treat MCP as a one-shot API call. Contenox does more: it keeps **persistent, session-scoped connections** to every registered MCP server.

- Each chat session gets its own dedicated connections.  
- State is preserved across all tool calls within that session.  

Your agent doesn't just call a tool—it builds a lasting relationship with it.

## Register an MCP server

```bash
# Shorthand: name + URL — transport defaults to http
contenox mcp add notion https://mcp.notion.com/mcp --auth-type oauth

# Local stdio server (spawned as child process)
contenox mcp add myfiles \
  --transport stdio \
  --command npx \
  --args "-y,@modelcontextprotocol/server-filesystem,/home/user/projects"

# Remote SSE endpoint with bearer auth
contenox mcp add memory \
  --transport sse \
  --url https://mcp.example.com/sse \
  --auth-type bearer \
  --auth-env MCP_TOKEN

# Remote HTTP endpoint with OAuth 2.1 (browser-based auth)
contenox mcp add notion https://mcp.notion.com/mcp --auth-type oauth

# Remote HTTP endpoint with injected context (hidden from model)
contenox mcp add internal \
  --transport http \
  --url http://internal-host:8090 \
  --header "X-Tenant: acme" \
  --inject "tenant_id=acme" --inject "env=production"
```

## OAuth authentication

Some hosted MCP servers (like Notion, Linear, Atlassian) require **OAuth 2.1** authorization — a browser-based flow that grants contenox an access token on your behalf.

**First-time setup:**

```bash
# 1. Register the server with oauth auth type
contenox mcp add notion https://mcp.notion.com/mcp --auth-type oauth

# 2. Run the auth flow (opens your browser automatically)
contenox mcp auth notion
```

After you approve the request in the browser, the token is stored in the local database and reused automatically for all subsequent connections. Tokens are refreshed transparently when they expire — you shouldn't need to run `mcp auth` again unless you revoke access.

**Re-authentication** (if a token is revoked or expired beyond refresh):

```bash
contenox mcp auth notion
```

### OAuth without dynamic client registration

Some MCP servers (HubSpot, Salesforce, Microsoft Graph, and most enterprise vendors) don't implement RFC 7591 dynamic client registration — you have to create the OAuth app manually in their developer UI to get a `client_id` and `client_secret`. Pass them to `mcp add` with `--oauth-client-id` and `--oauth-client-secret-env`:

```bash
# After creating an MCP Auth App in HubSpot's developer UI
# (redirect URL must be http://127.0.0.1:49152/callback)
export HUBSPOT_MCP_CLIENT_SECRET=<the client_secret>
contenox mcp add hubspot \
    --transport http --url https://mcp.hubspot.com/ \
    --auth-type oauth \
    --oauth-client-id <client_id from HubSpot> \
    --oauth-client-secret-env HUBSPOT_MCP_CLIENT_SECRET
contenox mcp auth hubspot
```

contenox stores only the env var name in its local SQLite, not the secret value. The secret is resolved from your environment at every connection. See the [HubSpot MCP recipe](/docs/use-cases/hubspot-mcp/) for the full walkthrough.



Manage servers like any other backend:

```bash
contenox mcp list
contenox mcp show myfiles
contenox mcp update myfiles --inject "tenant_id=newvalue"
contenox mcp remove myfiles
```

## Try a local stdio server

For a quick smoke test, register the official filesystem MCP server against a scratch directory:

```bash
mkdir -p /tmp/contenox-mcp-smoke
echo "hello from MCP" > /tmp/contenox-mcp-smoke/note.txt

contenox mcp add smoke-files \
  --transport stdio \
  --command npx \
  --args "-y,@modelcontextprotocol/server-filesystem,/tmp/contenox-mcp-smoke"

contenox mcp show smoke-files
contenox run "Use the smoke-files MCP tools to read note.txt and quote its contents."
```

This exercises the same MCP client path used by real stdio servers without requiring you to write or host a test server.

## Use MCP servers in a chain

Reference them by name in `execute_config.tools`:

```json
{
  "id": "ask_with_memory",
  "handler": "chat_completion",
  "system_instruction": "Available tools: {{toolservice:list}}.",
  "execute_config": {
    "model": "qwen2.5:7b",
    "provider": "ollama",
    "tools": ["myfiles", "memory"]
  }
}
```

The chain engine automatically connects to each server at task start and keeps the connections open for the entire session.

## Supported transports

| Transport | How it works                              | Best for                              |
|-----------|-------------------------------------------|---------------------------------------|
| `stdio`   | Spawns child process, communicates via stdin/stdout | Local tools (filesystem, databases) |
| `sse`     | Connects to remote Server-Sent Events endpoint | Cloud or shared team servers         |
| `http`    | Connects via HTTP streaming               | Production and internal services     |

## Subprocess lifetime & persistence

`stdio` servers are started when a `contenox` command begins and killed when it ends. This applies only to the `stdio` transport — HTTP and SSE servers are external processes you manage yourself and are unaffected. Each invocation is clean and reproducible, with no leftover server state from previous runs.

For state that must survive across runs, choose servers that persist on their own:
- `@modelcontextprotocol/server-memory` writes its graph to disk  
- Remote HTTP/SSE servers you manage can hold any state you need

## Injecting hidden parameters

You can inject key-value pairs into every MCP tool call — and they will be **completely invisible to the model**. The model's tool schema never shows them; Contenox merges them in silently on every call.

```bash
contenox mcp add myserver --transport http --url http://localhost:8090 \
  --inject "tenant_id=acme" \
  --inject "correlation_id=trace-xyz"

# Update inject params without recreating the server
contenox mcp update myserver --inject "tenant_id=newvalue"
```

Injected values always override any same-named args the model might provide. Use this for: tenant context, correlation IDs, session tags, environment identifiers, or any infrastructure parameter the model doesn't need to reason about.

For HTTP request headers (SSE/HTTP transports), use `--header` instead:

```bash
contenox mcp update myserver --header "X-Tenant: acme" --header "X-Version: 2"
```

> [!NOTE]
> `mcp update --header` and `mcp update --inject` each replace the **entire** corresponding map. Pass all required values in one call.

## Security notes

- Use `--auth-env` instead of `--auth-token` to keep secrets out of shell history.  
- `--inject` and `--header` values are stored in SQLite and **never logged or shown by `mcp show`** (keys are shown, values are masked).  
- `stdio` servers run as child processes—limit their filesystem access.  
- Session tokens are scoped to the current CLI session and never stored in plain text.

## Further reading

- [Official MCP specification](https://modelcontextprotocol.io/)  
- [MCP server registry](https://github.com/modelcontextprotocol/servers)  
- [CLI reference: `contenox mcp`](/docs/reference/contenox-cli#contenox-mcp)
