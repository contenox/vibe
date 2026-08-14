---
title: Workspace index & search
description: Build a local semantic index over your workspace and query it — from the CLI or from the agent's workspace_search tool.
---

# Workspace index & search

`contenox index` builds a semantic index over a workspace; `contenox search` asks it questions. Every answer is a ranked list of **file:line-range citations** with the matching text — a location you can open, not a floating blob. Retrieval is hybrid: an FTS5 full-text prefilter narrows the candidates, then embedding similarity (cosine) ranks them, so answers match by meaning rather than by keyword alone.

Everything stays local. Chunks, their embedding vectors, and the full-text index live in the shared SQLite database (`~/.contenox/local.db`), one index per workspace. The same index backs the agent's `workspace_search` tool, so a chain or session can query it too.

## Setup: an embedding model

Indexing and searching need an **embedding model**. Most chat models cannot embed, so set one explicitly:

```bash
contenox config set default-embed-model nomic-embed-text
contenox config set default-embed-provider ollama   # only if it differs from default-provider
```

For Ollama, pull the model first:

```bash
ollama pull nomic-embed-text
```

Embedding also works on OpenAI, Gemini, Vertex, and vLLM backends (with an embedding-capable model); Anthropic and Bedrock provide no embeddings.

> **Note:**
> Without `default-embed-model`, indexing falls back to the chat model — which embeds only on some providers. `contenox index` prints a warning when this fallback is active, and `contenox doctor` flags it too. Retrieval is optional: an agent that never searches stays fully functional with no embedding model at all.

## Build the index

```bash
contenox index                 # index the current directory's workspace
contenox index ~/src/project   # index a different directory
```

Indexing costs one embedding call per chunk — real money on a hosted provider. So `contenox index` always **plans first and shows the price before spending it**:

```
1,270 files → 7,065 chunks → 7,065 embed calls against nomic-embed-text · ollama

Make 7,065 embedding call(s) against nomic-embed-text · ollama? [y/N]
```

Nothing is embedded until you confirm. Pass `--yes` to skip the confirmation (required when stdin is not a terminal — scripts, CI); a closed stdin is never treated as "yes".

Refreshes are **incremental**: only files whose content changed are re-embedded, unchanged files reuse their existing chunks, and files that disappeared drop their chunks for free. Re-running `contenox index` on an unchanged tree costs nothing and says so. `--force` re-embeds every file instead.

Which files are indexed is the same set `@` completion and `find_files` see: gitignored paths, noise directories (`node_modules`, `dist`, …), binaries, oversized files, and dependency lockfiles are skipped, and the plan reports how many.

> **Note:**
> Changing the embedding model (or the chunking settings) creates a **new index generation** rather than mixing vectors from two models in one table — the plan announces this before the rebuild.

## Ask questions

```bash
contenox search "where is retry backoff configured"
contenox search "how does the approval flow work" --top 3
```

Each hit prints as `path:start-end`, a similarity score, and a short snippet. `--top` caps the number of hits (default 8, ceiling 50).

For scripting, `--json` emits the hits as JSON (an empty result is `[]`, never `null`):

```bash
contenox search "session storage" --json | jq -r '.[].path'
```

`contenox search` reads only the existing index — it never touches the filesystem live, so content added since the last `contenox index` is not there. What it *does* check live is staleness: a hit whose file changed (or vanished) since indexing is marked `STALE` instead of being served as current, with a reminder to refresh via `contenox index`.

## In chains and sessions

The agent reaches the same index through the `workspace_search` tool, provided by the `workspace` toolset. It is available in `contenox chat`, `contenox run`, and ACP editor sessions — add it to a task's tool allowlist like any other tool:

```json
"execute_config": {
  "model": "qwen3:8b",
  "provider": "ollama",
  "tools": ["workspace"]
}
```

The model passes a natural-language `question` (and optionally `top_k`) and gets back the same citations, each flagged when stale. A workspace with no index is not an error — the result tells the model to ask the operator to run `contenox index`. The tool is scoped to the session's own workspace; a model cannot name a different one.

The [HITL policy](/docs/guide/hitl/) treats `workspace_search` like any other tool call: the seeded presets allow it by default, and you can gate or deny it with a policy rule like anything else.

> **Note:**
> `workspace_search` answers by meaning and can be approximately right — it answers from retrieval, not from a parser, so treat a hit as a location to verify rather than a proof.

## Next

- [CLI reference: `contenox index` / `contenox search`](/docs/reference/contenox-cli/) — all flags
- [Config reference](/docs/reference/config/) — `default-embed-model` / `default-embed-provider`
- [Core concepts](/docs/guide/concepts/) — tool allowlists in `execute_config`
- [Local tools](/docs/integrations/tools/local/) — the `workspace_search` tool's parameters
- [HITL policies](/docs/guide/hitl/) — gating tool calls with approvals
