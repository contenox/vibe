---
description: Copy-paste chains for real work — commit messages, release notes, CRM writes, codebase docs, and browser automation.
---

# Cookbook

Production-ready, copy-paste patterns for automating real work with `contenox run` and friends.

Each recipe is a **pre-built solution**: pipe data in, the model executes your chain, you get structured output back. Stateless runs stay composable with the rest of your shell.

> **Prerequisites**
> - Run `contenox init` in your project once, then either point Contenox at a local Ollama model or configure a cloud backend — see [Quickstart](/docs/guide/quickstart/).
> - Use `--shell` for direct CLI recipes that need command execution; command policy lives in each chain’s `tools_policies`.

## Categories

- [Git & DevOps](/docs/use-cases/git-devops/) — commit messages, PR reviews, log summarization
- [Automated Release Notes](/docs/use-cases/release-notes/) — generate `RELEASE_NOTES.md` from `git log` using a chain pipeline
- [Stateful Agents with MCP](/docs/use-cases/stateful-agents-mcp/) — persistent memory across tool calls via MCP
- [Browser Automation with Playwright MCP](/docs/use-cases/playwright-mcp/) — drive a real browser with natural language
- [Notion as a Tool](/docs/use-cases/notion-mcp/) — create, search, and update Notion via OAuth MCP
- [Codebase Documentation](/docs/use-cases/codebase-docs/) — architecture guides from your source tree
- [Leads → HubSpot](/docs/use-cases/leads-to-hubspot/) — pipe a leads file into HubSpot CRM via an OpenAPI sub-spec
- [HubSpot via MCP](/docs/use-cases/hubspot-mcp/) — OAuth + pre-issued client credentials, works for any vendor MCP without dynamic registration
