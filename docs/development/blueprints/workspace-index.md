# contenox index / search — workspace retrieval over the existing seams

Decision record, 2026-07-27. Status: designed, awaiting build.
Supersedes ee-mining.md's "no retrieval story" gap with a shape that ports
nothing.

## Why, and the maintainer's framing

`contenox index` builds a workspace index; `contenox search "question"`
queries it; a local tool exposes the same query to the agent so it can ask
questions about the workspace. Maintainer, 2026-07-27: **"this should map
1:1 into everything we already do including the store interfaces etc — there
is zero new invention here."** That is the binding constraint, and it is
what separates this from the EE embed pipeline (mined and buried: a full
table scan with in-memory cosine, Postgres-shaped, tenant-scoped).

Every part already exists:

| Need | Existing seam |
| --- | --- |
| embeddings | `llmrepo.modelManager.Embed(ctx, EmbedRequest, prompt)` — provider-agnostic, already resolves through the model router |
| model/provider selection | `llmrepo.Config.DefaultEmbeddingModel` (+ the `contenox config` KV convention, `clikv`) |
| persistence | `internal/store/runtimetypes` + `libdbexec` — same store interface style as `jobqueue.go` / `kv.go` / `checkpoints.go`, SQLite |
| workspace scoping | `vfs` allowlist + the gitignore/skip-dir matcher `fileaddr`/`localtools` already share |
| tool surface | a local toolset registered in `localToolset` beside git/gointel, addressed by the envelope |
| CLI shape | the `mission`/`inbox` command-group pattern |

## Scope fence (what this is NOT)

Not a vector database. Not a service to run. Not multi-tenant. Not a
document-sync/connector product (EE's shape — buried). One local index per
workspace, in the same SQLite file everything else uses.

## Design

**Config (new KV keys, `contenox config set`):** `default-embed-model`,
`default-embed-provider`. **Prerequisite fix:** `enginesvc.Build` currently
sets `DefaultEmbeddingModel` to the CHAT model
(`internal/kernel/enginesvc/engine.go:170`) — correct only where a provider's
chat model also embeds. The engine must read the embed keys, falling back to
the chat model with a warning, and `contenox doctor`/setupcheck should report
an unset/unembeddable embed model as a WARNING (not a blocker: retrieval is
optional).

**Chunking:** files from the vfs-scoped walk through the shared noise filter
(gitignore + skip-dirs — the same matcher `find_files` and `@`-completion
use, so human, agent and index agree on what exists). Line-oriented chunks
with overlap, sized in tokens against the embed model's limit; each chunk
keeps `{path, startLine, endLine, sha}` so a hit is a citation
(`file:line-range`), never a floating blob.

**Store** (`runtimetypes`, mirroring `jobqueue.go`'s style): an index-config
row (embed model, provider, dimension, chunking params, created-at) and a
chunk row (workspace, path, line range, content sha, text, vector blob).
**Create-once immutability** — the one idea banked from EE
(`indexconfig`): changing the embed model does not mutate an index; it makes
a new one and cuts over. Prevents the silent-corruption failure where
vectors from two models share a table.

**Search:** cosine over the workspace's chunks, top-K, with a lexical
pre-filter (SQLite FTS5 — verified available in the driver already in the
module, per ee-mining) so scoring never scans everything. Hybrid retrieval
by construction: FTS5 narrows, vectors rank. V1 accepts a linear scan over
the narrowed set; ANN is a later optimization gated on measured pain, not
assumed.

**Incremental:** re-index touches only files whose sha changed
(git-status-style); `contenox index --force` rebuilds. Deleted files drop
their chunks.

**Surfaces:**
- `contenox index [dir] [--force]` — build/refresh, progress to stderr,
  honest counts.
- `contenox search "question" [--top N] [--json]` — the operator's read.
- `workspace_search` local tool (provider `workspace`, allow-tier — it is a
  read of files the agent may already read) returning ranked
  `file:line-range` citations plus the chunk text, capped.

**Relationship to gointel:** complementary, not competing. gointel answers
structural questions about Go (who calls this, what type is this) with exact
truth. The index answers semantic questions across everything (docs,
markdown, configs, other languages): "where is retry backoff explained".
Both are reads; both cite locations.

## Risks

- **Embed model availability** — many local setups have no embedding model
  pulled. Everything degrades: no index ⇒ the tool reports "no index for
  this workspace; run contenox index", never a hard failure.
- **Index staleness** — a hit whose file changed underneath is a lie. Chunks
  carry the content sha; a hit whose sha no longer matches is marked stale
  in the result rather than silently returned.
- **Cost/time on big repos** — one embed call per chunk. Bounded by the
  noise filter, a file-size cap, and a documented "this will take N calls"
  pre-count before starting.
- **Scope creep into a RAG product** — the fence above is the answer;
  ee-mining's verdict on the EE pipeline is the precedent.

## Build order

1. config keys + the `DefaultEmbeddingModel` wiring fix + doctor warning
2. store types (index config, chunks) + create-once discipline
3. chunker + indexer service (`internal/services/workspaceindex`)
4. search (FTS5 pre-filter + cosine rank) + staleness marking
5. CLI verbs, then the local tool + envelope rule
6. adversarial review: staleness, containment, cost bounds
