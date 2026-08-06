---
title: Browser Automation with Playwright MCP
description: Register Microsoft's Playwright MCP server and drive a real browser from the terminal — navigation, clicks, snapshots, and more.
---

# Browser Automation with Playwright MCP

Control websites with plain English. Add the official [`@playwright/mcp`](https://www.npmjs.com/package/@playwright/mcp) server once and your local Contenox workflow can drive a real browser: navigate, click, type, take snapshots, read structured accessibility trees, and extract data — all without sending page pixels to a vision model (unless you explicitly use screenshot-related tools). Use it for scraping, testing, form filling, or research workflows.

## Prerequisites

- `contenox init` in your project and a configured backend (see the [Quickstart](/docs/guide/quickstart)).
- [Node.js](https://nodejs.org/) with `npx` available (the MCP server runs via `npx`).

## One-command setup

Register the Playwright MCP server (stored in Contenox’s SQLite DB — survives reboots):

```bash
contenox mcp add playwright --transport stdio \
  --command npx --args "-y,@playwright/mcp@latest"
```

## Expose tools to `contenox run`

The default **run** chain from `contenox init` exposes registered tools automatically via `"tools": ["*"]`, so Playwright is available as soon as you add the server.

If you want tighter control, replace `"*"` with an explicit allowlist in `.contenox/chain-agent-run.json`, or use a custom chain whose `tools` include only the tools you want.

> **Tip:**
> Tool names exposed to the model are prefixed with the server name, e.g. `playwright.<tool_name>`.

For tighter control before sensitive actions, use explicit `tools_policies` in your chain. HITL approval is on by default; pass `--auto` only for trusted, unattended runs.

## Run it

```bash
# Simple navigation + summary
contenox run "Use the Playwright MCP tools to go to https://github.com/microsoft/playwright-mcp and summarize the latest release."

# Research flow
contenox run "Use the Playwright MCP tools to open https://x.ai, find the latest Grok announcement, and list the key points."

# Search and extract
contenox run "Use the Playwright MCP tools to search Google for 'contenox github', open the first result, and report the current star count."
```

## Advanced `mcp add` options

Append extra flags to the Playwright MCP package via comma-separated `--args` (each segment becomes one argument):

| Goal | Example |
|------|---------|
| Headless | `--args "-y,@playwright/mcp@latest,--headless"` |
| Specific browser | `--args "-y,@playwright/mcp@latest,--browser,firefox"` |
| Unrestricted file access | `--args "-y,@playwright/mcp@latest,--allow-unrestricted-file-access"` |
| Persistent profile (logins/cookies between runs) | `--args "-y,@playwright/mcp@latest,--user-data-dir,$HOME/.playwright-mcp"` |

See the package README on [npm](https://www.npmjs.com/package/@playwright/mcp) for the full CLI surface.

## How it works

1. **Registration** — `contenox mcp add` stores the server in SQLite; a worker keeps a session so tools stay responsive across steps.
2. **Tools** — The model receives Playwright MCP tools (namespaced with the server name). Prefer accessibility snapshots and DOM-driven actions; screenshot tools are available when you need them.
3. **Local execution** — The browser runs on your machine; no cloud browser farm is required.
4. **Safety** — Treat this like any automation with network and filesystem access: use trusted sites, review chains, and keep HITL enabled unless you deliberately need unattended `--auto` runs.
