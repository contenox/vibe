# Contenox Local – Plan

**Goal:** Run the workflow engine locally with minimal infra: SQLite, no NATS, tokenizer replaced by estimates. One binary (or CLI) that runs workflows and does side effects on the local machine.

---

## Keep the API (server) version working

**Requirement:** The existing API server (`cmd/runtime-api`, Postgres + NATS + tokenizer service) must keep working unchanged. No regressions, no “if local then …” in shared code.

**How we achieve it:**

| Principle | What we do |
|-----------|------------|
| **Additive only** | We add new files and a new entrypoint. We do not modify server-only code paths. |
| **New implementations, same interfaces** | SQLite implements `DBManager`; in-mem implements `Messenger`; estimate implements `Tokenizer`. The server keeps using Postgres, NATS, and HTTP tokenizer. No changes to `cmd/runtime-api` or to how it constructs dependencies. |
| **Two entrypoints** | `cmd/runtime-api` = server (unchanged). `cmd/contenox-local` = local (new). Each builds its own dependency graph; shared packages (runtimestate, downloadservice, llmrepo, taskengine) receive different implementations via constructor args only. |
| **No “mode” in shared code** | We do not add `if local { ... } else { ... }` inside runtimestate, downloadservice, llmrepo, etc. All branching is at the top level (which main is running). Shared code stays mode-agnostic. |
| **Server stays on current stack** | Postgres (`libdbexec/postgres.go`), NATS (`libbus/nats.go`), HTTP tokenizer (`ollamatokenizer.NewHTTPClient`), existing `serverapi` and compose. Zero changes to those for the local work. |

**Concrete:**

- **libdbexec:** Add `sqlite.go`; leave `postgres.go` and all existing call sites (server) untouched.
- **libbus:** Add `inmem.go`; leave `nats.go` and server’s `initPubSub` untouched.
- **ollamatokenizer:** Add `estimatetokenizer.go`; leave HTTP client and server’s tokenizer init untouched.
- **cmd/runtime-api/main.go:** No edits. It keeps calling the same constructors with the same config (Postgres DSN, NATS URL, tokenizer URL).
- **Schema:** Add a SQLite-specific schema (e.g. `schema_sqlite.sql` or `SchemaSQLite`) used only when opening SQLite. Existing `runtimetypes.Schema` (Postgres) and server init stay as-is.

With this, the API version keeps working; local is a parallel path that reuses the same engine behind different infra.

---

## 1. SQLite instead of Postgres

**Why:** Single file, no server, perfect for local. Already agreed as the main persistence swap.

**Current:** `libdbexec` has `DBManager` interface; only Postgres implementation exists. Schema lives in `runtimetypes/schema.sql` (Postgres: `JSONB`, `TIMESTAMP`, etc.).

**Work:**

| Task | Effort | Notes |
|------|--------|--------|
| Add `libdbexec/sqlite.go` implementing `DBManager` (WithTransaction, WithoutTransaction, Close) | Small | Use `modernc.org/sqlite` or `github.com/mattn/go-sqlite3` (cgo). |
| SQLite-compatible schema | Medium | Current schema uses `JSONB`, `REFERENCES ... ON DELETE CASCADE`. SQLite: use `TEXT` for JSON (or SQLite 3.38+ JSON), keep FKs. Either a second file `schema_sqlite.sql` or build-time/dialect switch. |
| Wire schema init to SQLite | Small | Same `runtimetypes.Schema` pattern or `runtimetypes.SchemaSQLite`; init on first run (e.g. `~/.contenox/local.db` or `./.contenox/local.db`). |
| Parameter placeholders | Check | Go `database/sql` with SQLite driver: often `?` instead of `$1`. If all queries use `$1`-style, we need a mapper or to use a driver that accepts `$1` (e.g. mattn/go-sqlite3 with `sqlite3_enable_load_extension` or a thin wrapper that rewrites placeholders). **Audit:** grep shows `$1`-style in runtimetypes, eventstore, functionstore, etc. So either: (a) add a small adapter that rewrites `$N` → `?` and renumbers for SQLite, or (b) use a driver that supports `$N` (e.g. modernc.org/sqlite supports both). |

**Risk:** Low. Interface is clear; main work is schema dialect and placeholder handling.

---

## 2. Kill NATS – in-memory Messenger

**Why:** For local single-process we don’t need a broker. All bus usage is in-process (download queue progress + cancel).

**Current:** `libbus.Messenger` (Publish, Stream, Request, Serve, Close). Only NATS implementation. Used by:
- **runtimestate/state.go:** `Stream("queue_cancel", ch)`, `Publish("model_download", message)`
- **downloadservice:** `Publish("queue_cancel", b)`, `Stream("model_download", ch)`

So two subjects, same process. In-memory delivery is enough.

**Work:**

| Task | Effort | Notes |
|------|--------|--------|
| Add `libbus/inmem.go` (or `local.go`) | Small | Implement `Messenger`: map of subject → list of channels for Stream; Publish fans out to subscribers; Request/Serve: same-process request-reply (map subject → handler, Request sends and blocks on reply channel). Mutex for concurrent Publish/Subscribe. |
| Make bus choosable at startup | Small | Local entrypoint passes `libbus.NewInMem()` (or similar); server entrypoint keeps `libbus.NewPubSub(ctx, natsConfig)`. No change to runtimestate or downloadservice. |
| Optional: allow nil bus for “run one chain, no download” | Tiny | If we ever want a mode with no download queue at all, we could allow nil and no-op Publish/Stream in a wrapper; not required for first cut. |

**Risk:** Low. Interface is small; in-mem implementation is straightforward.

---

## 3. Tokenizer → estimates

**Why:** Remove dependency on tokenizer service (Ollama tokenizer HTTP). For local, “good enough” token counts are fine for context-window checks.

**Current:** `ollamatokenizer.Tokenizer` (Tokenize, CountTokens, OptimalModel). Used by:
- **llmrepo:** CountTokens (and Tokenize only via adapter). **taskexec** only calls CountTokens (context trimming, logging).
- Mock already exists: word-split count for CountTokens, returns baseModel for OptimalModel.

**Work:**

| Task | Effort | Notes |
|------|--------|--------|
| Add `ollamatokenizer/estimatetokenizer.go` | Small | Implement `Tokenizer`: **CountTokens:** e.g. `len(text)/4` (classic) or `utf8.RuneCountInString(text)/4`; or slightly better: words * 1.35. **Tokenize:** return dummy slice of length = estimated count (needed for interface; no caller in taskexec uses actual token IDs). **OptimalModel:** return `baseModel` unchanged (no proxy model). |
| Local entrypoint uses EstimateTokenizer | Tiny | Same pattern as playground `WithMockTokenizer()`: pass estimate tokenizer into llmrepo.NewModelManager. No HTTP tokenizer URL. |
| Optional: make tokenizer optional in llmrepo | Small | Currently llmrepo requires non-nil tokenizer. For local we always pass estimate; if we ever want “no tokenizer” we could allow nil and have CountTokens return 0 or estimate in the adapter. Not required for first cut. |

**Risk:** Low. Slight under/over count vs real tokenizer; acceptable for local context-window and logging.

---

## 4. New entrypoint: “local” runner

**What:** A mode or binary that (a) uses SQLite, (b) uses in-memory bus, (c) uses estimate tokenizer, (d) reads config (e.g. one Ollama URL + model) from file/env, (e) runs one or more workflows (file/stdin) and exits (or stays up for events if we add that later).

**Work:**

| Task | Effort | Notes |
|------|--------|--------|
| Add `cmd/contenox-local/main.go` (or `contenox run` under a single CLI binary) | Medium | Parse flags: config path, workflow file (or stdin). Load config (YAML/JSON): base_url, model, maybe API keys. Init: SQLite DB (libdbexec), InMem bus (libbus), EstimateTokenizer, runtimestate.New(..., withGroups or not), backends/models from config (or single “default” backend in DB). No HTTP server unless we add “webhook receiver” later. |
| Static backend/model config | Small | Either: (1) seed SQLite with one backend + one model from config on first run, or (2) add a “static resolver” path that doesn’t use DB (bigger change). (1) is simpler: one row in llm_backends, one in ollama_models, assign to default group. |
| Run one chain | Medium | Build exec service + task env (same as server path), load chain from file or stdin, call taskService.Execute(ctx, chain, input, inputType), print output + optional state. |
| Optional: Valkey | Low | If anything in the stack still expects Valkey (cache, sessions), we can use in-memory or skip for local. Grep suggests Valkey is in libkvstore; if only used for cache we can provide an in-memory impl or stub for local. |

**Risk:** Medium only because of surface area (wiring all services). No new engine logic.

---

## 5. Order of work (suggested)

1. **In-memory bus** – Unblocks local mode without NATS; no schema or driver work.
2. **Estimate tokenizer** – Drop tokenizer-service dependency for local; small, self-contained.
3. **SQLite** – Schema variant + libdbexec/sqlite.go + placeholder handling. Enables “real” persistence for local (backends, chains, queue, etc.).
4. **Local entrypoint** – Wire everything: SQLite + InMem bus + EstimateTokenizer + config-based backend/model, then run one chain from file/stdin.

After that: optional extras (CLI polish, `contenox run`, watch mode, webhook listener, Valkey stub).

---

## 6. What we’re not changing (for now)

- **Task engine** – Unchanged.
- **Event store / event bridge / JS functions** – Can stay on SQLite; same schema migration. We can defer “event-driven local” to a second phase (e.g. file watcher or local webhook).
- **Server binary and wiring** – Unchanged. `cmd/runtime-api` keeps Postgres + NATS + real tokenizer; no edits to server main or serverapi. Local is an additional entrypoint only.
- **OpenAPI / HTTP API** – Not used by local runner in the minimal version.

---

## 7. Summary table

| Change | Delivers | Effort |
|--------|----------|--------|
| SQLite | No Postgres, single-file DB | Medium (schema + driver + placeholders) |
| In-memory Messenger | No NATS | Small |
| Estimate tokenizer | No tokenizer service | Small |
| Local entrypoint | Single binary, run workflow from file | Medium |

**Feasibility:** High. All four are contained and the engine stays as-is.
