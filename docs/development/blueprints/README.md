# Blueprints

Design records, decision documents, and R&D directions for the Contenox
runtime. Blueprints capture the *why* behind the implementation; user-facing
how-to docs live one level up in `docs/`.

## Active subsystems

| Area | What it covers |
| --- | --- |
| [acp/](acp/README.md) | Agent Client Protocol surface: contenox as agent (registry submission artifacts, sandbox architecture) and as client (the client-side engine, fleet and mission machinery) |
| [providers/](providers/README.md) | Cloud/hosted provider integrations |

## Beam TUI and engine designs

| Doc | Status | What it covers |
| --- | --- | --- |
| [beam-tui.md](beam-tui.md) | active blueprint | The beam TUI component blueprint: constitutional decisions, build order, testability doctrine |
| [gointel.md](gointel.md) | active blueprint | In-process Go code intelligence for the agent: design, prior-art evidence, measurement plan |
| [goja-tools.md](goja-tools.md) | active blueprint | Script tools and a sandbox tool over one bounded goja runtime: limits, the host.tool boundary, naming rule |
| [workspace-index.md](workspace-index.md) | active blueprint | `contenox index` / `search` and the workspace_search tool over the existing embed, store and vfs seams |
| [ee-mining.md](ee-mining.md) | mining report | The EE graveyard expedition: per-finding port verdicts (goja, embed pipeline, BM25 grounding), banked facts, leave-buried list |
