---
description: Recipes for real work — MCP integrations, browser automation, authored HITL policies, and tool-authoring patterns.
---

# Cookbook

Production-ready patterns for automating real work with contenox agents.

Each recipe is a **pre-built solution**: a chain or agent declaration that does the work and hands back structured output.

Most recipes here are written as chain files rather than [agent declarations](/docs/guide/agents/), because a recipe is a pipeline you point at — a fixed shape you can diff — rather than an agent you fire at an intent. Where a recipe needs branching, a per-step model, or a declared human gate, the chain is the only tier that says so.

> **Prerequisites**
> - Run `contenox init` in your project once, then either point Contenox at a local Ollama model or configure a cloud backend — see [Quickstart](/docs/guide/quickstart/).
> - Tool access — `local_shell`, `local_fs`, and any MCP servers or registered remote tools — is governed by each chain's `tools_policies`.

Several recipes below are one branch of a router rather than a standalone chain: a `route` task labels the request and hands it to the loop built for that label, with its own instruction, tool scope and budget. See [Request routing](/docs/guide/chains/routing/) for the shape.

## Categories

- [Stateful Agents with MCP](/docs/use-cases/stateful-agents-mcp/) — persistent memory across tool calls via MCP
- [Browser Automation with Playwright MCP](/docs/use-cases/playwright-mcp/) — drive a real browser with natural language
- [Notion as a Tool](/docs/use-cases/notion-mcp/) — create, search, and update Notion via OAuth MCP
- [The review specialist](/docs/use-cases/review-specialist/) — "review the git diff" answered with a restatement, until a third branch in the router gave the request its own loop, with the write tools taken away
- [The moderation gate](/docs/use-cases/moderation-gate/) — a cheap model classifies each message safe or unsafe before the expensive one runs
- [Multi-provider fallback](/docs/use-cases/multi-provider-fallback/) — a candidate list of models and providers with a retry policy, so a rate limit or outage falls through instead of failing
- [Any API, a tool you authored](/docs/use-cases/any-api-as-a-tool/) — register an HTTP API as a tool with the credential hidden from the model and the endpoint surface narrowed
- [Authoring your tool inventory](/docs/use-cases/openapi-subset/) — cut a large OpenAPI spec down to a hand-curated subset and register only that
- [The pause is yours to define](/docs/use-cases/authored-approval/) — write a HITL policy that pauses only on the tool calls you name, then activate it
- [The nested permission bomb](/docs/use-cases/nested-permission-bomb/) — give a workflow its own scoped credential and authored HITL policy instead of inheriting the operator's access
- [HubSpot via MCP](/docs/use-cases/hubspot-mcp/) — OAuth + pre-issued client credentials, works for any vendor MCP without dynamic registration
- [The oracle](/docs/use-cases/auto-attention/) — an adjudicating agent rules on a subagent's routine asks so unattended runs finish; consequential ones still wait for you
