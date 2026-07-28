---
description: Connect contenox to your Notion workspace via the official MCP server — read, create, and update pages like any chat message.
---

# Notion as a Tool

Connect `contenox` to your Notion workspace via the official Notion MCP server. The model can read, create, and update Notion pages as naturally as answering a chat message — no scripts, no SDKs.

## Prerequisites

**One-time setup:** register the Notion remote MCP server and authenticate:

```bash
# Register the official Notion MCP remote server (HTTP transport + OAuth)
contenox mcp add notion --transport http \
  --url https://mcp.notion.com/mcp \
  --auth-type oauth

# Authenticate (opens a browser for OAuth consent)
contenox mcp auth notion
```

> [!NOTE]
> Notion's MCP server uses OAuth 2.0 with dynamic client registration. `contenox mcp auth` handles the full flow — discovery, registration, authorization code exchange, and token storage — automatically. Tokens are refreshed transparently on each run.
>
> Official docs: [Notion MCP server guide](https://developers.notion.com/guides/mcp/mcp)

---

## Recipe 1: List recent pages

```bash
contenox run "Use the Notion MCP tools to list my recent Notion pages on the topic software development."
```

**Example output:**

> Your recent Notion pages:
> - **AI Usage in Software Development** — created today
> - **Sprint Planning Q2** — last edited 2 days ago
> - **Team OKRs** — last edited 1 week ago

---

## Recipe 2: Create a page with scaffolded content

```bash
contenox run "Use the Notion MCP tools to create a Notion page and scaffold an article on AI usage in software development."
```

The model calls `create_page`, writes the title, and populates the body with a structured outline covering introduction, key benefits, tooling, challenges, and conclusion — returning the live URL.

**Example output:**

> I have created a Notion page titled "AI Usage in Software Development" with a scaffolded outline.
> You can access and edit it here: https://www.notion.so/3240ef89...

---

## Recipe 3: Search and summarize

```bash
contenox run "Use the Notion MCP tools to search my recent Notion pages for 'roadmap' and give me a bullet-point summary."
```

The model calls `search_pages`, fetches matching content, and summarizes it inline — no copy-pasting required.

---

## Recipe 4: Pipe a draft into Notion

```bash
cat my-draft.md | contenox run "Use the Notion MCP tools to create a Notion page with this content, title it 'Draft: $(date +%F)'."
```

Combine stdin piping with Notion write access to push any local file directly into your workspace.

---

## How it works

These recipes work with `contenox run`. The default run chain (`.contenox/default-run-chain.json`) is configured with `"tools": ["*"]`, so registered MCP servers such as Notion are available to the model automatically.

```json
{
  "tasks": [{
    "handler": "chat_completion",
    "system_instruction": "You are Contenox, a helpful AI assistant. Answer the user's questions clearly and helpfully. If you have tools available, use them when appropriate.",
    "execute_config": {
      "tools": ["*"],
      "pass_clients_tools": false
    }
  }]
}
```

- `tools: ["*"]` — exposes all registered MCP servers to the model. Add `"!name"` entries to exclude specific servers (e.g. `["*", "!filesystem"]`).
- A bare `contenox "..."` command is session-backed chat (injected as `chat`) and uses `default-chain.json`; only `contenox run` is the stateless path that uses `.contenox/default-run-chain.json`.
- The task engine handles the full tool-call loop automatically: model calls a tool → result appended to history → model continues.

> [!TIP]
> Add `--trace` to watch every Notion API call, its arguments, and the raw results in real time.

---

## Scope and limitations

Notion's MCP server exposes the 14 tools from its [official MCP guide](https://developers.notion.com/guides/mcp/mcp) — including `search`, `create_page`, `update_page`, `retrieve_block_children`, and more. Database queries, page updates, and rich block types are all supported.

Access is scoped to the pages and databases the user shared with the integration during OAuth consent.
