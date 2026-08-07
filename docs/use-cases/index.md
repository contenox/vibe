---
description: Copy-paste chains for real work — commit messages, release notes, CRM writes, codebase docs, and browser automation.
---

# Cookbook

Production-ready, copy-paste patterns for automating real work with `contenox run` and friends.

Each recipe is a **pre-built solution**: pipe data in, the model executes your chain, you get structured output back. Stateless runs stay composable with the rest of your shell.

> **Prerequisites**
> - Run `contenox init` in your project once, then either point Contenox at a local Ollama model or configure a cloud backend — see [Quickstart](/docs/guide/quickstart/).
> - Use `--shell` for direct CLI recipes that need command execution; command policy lives in each chain’s `tools_policies`.

Several recipes below are one branch of a router rather than a standalone chain: a `route` task labels the request and hands it to the loop built for that label, with its own instruction, tool scope and budget. See [Request routing](/docs/guide/request-routing/) for the shape.

## Categories

- [Git & DevOps](/docs/use-cases/git-devops/) — commit messages, PR reviews, log summarization
- [Automated Release Notes](/docs/use-cases/release-notes/) — generate `RELEASE_NOTES.md` from `git log` using a chain pipeline
- [Stateful Agents with MCP](/docs/use-cases/stateful-agents-mcp/) — persistent memory across tool calls via MCP
- [Browser Automation with Playwright MCP](/docs/use-cases/playwright-mcp/) — drive a real browser with natural language
- [Notion as a Tool](/docs/use-cases/notion-mcp/) — create, search, and update Notion via OAuth MCP
- [Codebase Documentation](/docs/use-cases/codebase-docs/) — architecture guides from your source tree
- [The review specialist](/docs/use-cases/review-specialist/) — "review the git diff" answered with a restatement, until a third branch in the router gave the request its own loop, with the write tools taken away
- [The moderation gate](/docs/use-cases/moderation-gate/) — a cheap model classifies each message safe or unsafe before the expensive one runs
- [Multi-provider fallback](/docs/use-cases/multi-provider-fallback/) — a candidate list of models and providers with a retry policy, so a rate limit or outage falls through instead of failing
- [Any API, a tool you authored](/docs/use-cases/any-api-as-a-tool/) — register an HTTP API as a tool with the credential hidden from the model and the endpoint surface narrowed
- [Authoring your tool inventory](/docs/use-cases/openapi-subset/) — cut a large OpenAPI spec down to a hand-curated subset and register only that
- [The pause is yours to define](/docs/use-cases/authored-approval/) — write a HITL policy that pauses only on the tool calls you name, then activate it
- [The nested permission bomb](/docs/use-cases/nested-permission-bomb/) — give a workflow its own scoped credential and authored HITL policy instead of inheriting the operator's access
- [Leads → HubSpot](/docs/use-cases/leads-to-hubspot/) — pipe a leads file into HubSpot CRM via an OpenAPI sub-spec
- [HubSpot via MCP](/docs/use-cases/hubspot-mcp/) — OAuth + pre-issued client credentials, works for any vendor MCP without dynamic registration
- [Auto-attention mode (beta)](/docs/use-cases/auto-attention/) — an oracle chain answers routine mission questions so unattended runs finish; consequential asks still wait for you
