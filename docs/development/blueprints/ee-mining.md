# ee-mining — what the EE graveyard holds, and what we take from it

Mining report, 2026-07-27. Status: assessment only. Nothing has been ported;
the source repo was read-only throughout.

Source: the enterprise monorepo at `/home/naro/src/github.com/enterprise` —
~66k LOC of Go across four modules (`blueprints`, `bob2`, `runner`,
`vald-operator`) plus a Next.js site. Paths written `ee/…` are relative to
that root; unprefixed paths are this repo. Its own `runtime/` is an
unpopulated submodule pointing back at us, so nothing here is a fork of us.

## The verdict, up front

**Nothing ports as-is. One mechanism is worth lifting when a need names
itself; one is worth banking as a decision-already-made; everything else
stays buried.**

The graveyard's shape explains why. EE is a *server* — multi-tenant,
Postgres+Helm+k8s, document ingestion for a hosted search product. We are a
single-machine coding harness whose identity is the envelope. The two
codebases overlap on exactly one axis (retrieval over local files), and on
every axis where they *appear* to overlap (sandboxing, retry, telemetry,
tool infrastructure), we are already ahead — often because EE deliberately
delegated that half to us. `ee/docs/01-pivot.md` documents this from the
other side: every platform primitive was deleted from the runtime and every
standard-protocol primitive added. Mining the platform side for harness
mechanisms mostly finds the things that were correctly deleted.

Three sections: the two named targets, then the open sweep, then the ranked
shortlist and the leave-buried list.

---

## 1. The goja engine

goja survives in EE in two live places and one buried one.

| Artifact | Location | Size | Tests |
| --- | --- | --- | --- |
| Event-function executor | `ee/bob2/internal/functionexec/` | 232 LOC (`goja.go` 143, `builtins.go` 89) | `goja_test.go`, 61 LOC, 5 tests |
| Expression conformance host | `ee/blueprints/expr/` | 290 Go + `expr.js` 285 JS | `expr_test.go` 230 LOC + `fixtures.json` (16.6 KB) + `conformance.node.js` |
| The real one — deleted | `ee/bob/jseval/` + `ee/bob/localhooks/jssandboxhook.go`, recoverable at `git show 31ab41b^:…` | ~1,100 LOC + 305 LOC hook | `builtins_test.go` 109, `jssandboxhook_test.go` 223 |

### 1a. `functionexec` / `jseval` as code — **DON'T**

What it does: `GojaExecutor.ExecuteFunction` compiles a script (cached by
SHA-1 of source in a `sync.Map`), spins a **fresh `goja.Runtime` per
execution** so globals cannot leak between functions, injects an event
object, runs, and exports a JSON-serialisable `map[string]any`. Two builtins:
a no-op `console` and `httpFetch` capped at 8 MiB. The historical `jseval`
added a `Builtin` interface, a `Collector` that records console output and
builtin calls as structured entries, `GetBuiltinSignatures()` returning
`taskengine.Tool` descriptions of the sandbox API, and an `executeHook`
bridge into `HookRepo`.

**The reusable content is about forty lines** — fresh VM, hash-keyed program
cache, JSON export — and **every hard part is missing**. There is no
`vm.Interrupt()` call anywhere in any of the three artifacts (verified by
grep across `jseval.go`, `builtins.go`, `httpfetch.go`, `collector.go`,
`builtin.go`, `jssandboxhook.go`). goja exposes `Interrupt` /
`ClearInterrupt` / `SetMaxCallStackSize`; none is used. The historical
`jseval.RunProgram` *looks* bounded — it selects on `ctx.Done()` — but it
never interrupts the VM, so a cancelled `while(true){}` returns
`context.Canceled` to the caller and **leaks the VM goroutine for the life of
the process**. `functionexec` does not even have that: `ctx` reaches the
builtins and nothing else. `builtins.go:21` admits the other hole in a code
comment: "httpFetch can reach any URL the bob process can — SSRF hardening
(allowlist / block link-local) is a follow-up." Test coverage is
happy-path plus compile-error and throw; no test runs an infinite loop, an
allocation bomb, or a deep recursion.

Porting this buys a forty-line skeleton and inherits two bugs. If we want
JS, we write it.

### 1b. A bounded `js_eval` compute tool, built fresh — **RETHINK-SHAPE, gated**

The named problem is thin but real: to compute anything deterministic — parse
a JSON blob, do arithmetic over a file's numbers, reshape a list — the agent
today either does it in-head or shells out. `local_shell`'s allow tier
(`internal/services/hitlservice/policy.go:949`) is
`git status,git diff,…,ls,cat,head,tail,wc,pwd,echo,grep,rg` — no `python`,
no `node`, no `jq` — so every transform is an approval prompt. A compute tool
that cannot touch fs or net is the textbook allow-tier candidate.

But it is **not currently a named problem**: no TODO entry, no blueprint, no
dogfooding note asks for it. Under the house rule it therefore waits until
dogfooding makes us miss it. Recording the shape now so the decision is cheap
later:

- **Target:** `internal/services/localtools/jseval.go`, one tool `js_eval`
  under a new `compute` namespace, `taskengine.ToolsRepo` like
  `echo.go`/`git.go`. No engine changes, no new handler.
- **Deliberately dropped from the mined version:** `httpFetch` (the only
  builtin worth having is the one we must not have — `webtools` already does
  HTTP under policy), the event injection and `eventstore` coupling, the
  `executeHook` bridge, the compiled-program cache (single-shot calls never
  repeat), the `Collector` and its tracker plumbing, `GetBuiltinSignatures`.
  What survives is: fresh VM, run, export JSON, cap output.
- **What must be written that the mine does not have:** a watchdog that calls
  `vm.Interrupt()` on wall-clock deadline *and* on `ctx.Done()` (not a select
  around the goroutine), `SetMaxCallStackSize`, and an output byte cap in the
  house style (truncate with a resume notice, never silently —
  `localtools/hardening.go` Rec 4).

**The blocker, and it is the finding that matters here:** "allow-tier by
construction" holds for fs and net, and **fails for memory**. goja has no
memory limit — none in the API, verified against
`dop251/goja@v0.0.0-20260311135729`. `new Array(1e9).fill(0)` OOMs the
harness process. Nothing else in beam bounds memory either: `libsandbox` is
landlock + netns/tun + a seccomp user-notify telemetry tap — filesystem and
network confinement, no rlimits, no cgroups (grep for
`Rlimit|RLIMIT|cgroup` in `internal/libsandbox/` returns nothing), and the
envelope's `ComputeBounds` (`docs/development/blueprints/acp/envelope-compute-bounds.md`)
bounds turns, tool calls, and tokens — spend, not RSS. So a js_eval tool at
allow tier is a tool that can kill the process without an approval prompt.
Either that is stated plainly in the tool description and accepted, or
js_eval runs out-of-process under an rlimit — which is a new subprocess
lifecycle, and at that price `python3 -c` under the existing shell policy is
the cheaper tool.

**Dependency cost, measured** (isolated module, `go build`): goja adds four
modules — `dlclark/regexp2`, `go-sourcemap/sourcemap`, `google/pprof`,
`golang.org/x/text` — of which `pprof` and `x/text` are already indirect
here (`go.mod:71`, `go.sum:355`). Binary delta measured in isolation:
**13.3 MB vs a 2.4 MB baseline, ≈ +11 MB**, against a current `contenox`
binary of 53.9 MB — roughly +20%. That is a real number for a tool nobody
has asked for yet.

### 1c. Scriptable chain hooks — **DON'T** (identity)

`ee/bob/localhooks/jssandboxhook.go` is the mechanism the maintainer flagged
as an identity question. It is genuinely well-shaped: a chain task generates
JS, the next task runs it under `hook.Name = "js_execution"`, and the hook
returns `{ok, error, result, logs}` **without failing the chain**, so a
compile error or a throw comes back as data the model can correct from. That
errors-are-feedback-not-failure discipline is good, and it is already ours in
a different clothing: `localtools/hardening.go` (severity markers,
did-you-mean suggestions, never-truncate-silently) is the same idea applied to
tools rather than scripts.

The answer is still no, on two grounds. First, this repo has **no
`HookRepo`** — the whole hook extension point was deleted in the reshape
(`ee/docs/01-pivot.md` lists `jseval`, `localhooks`, `internal/hooks`,
`hookrecipes`, `jssandboxhook.go` under "sandboxed user code as the
customization mechanism", all removed) and the handler vocabulary is now a
**closed contract table**: `internal/kernel/taskengine/handler_signatures.go`
freezes each handler's I/O so `chainlint.go` can reject an impossible edge
before anything runs. A `js` handler reopens that vocabulary to arbitrary
runtime-typed output and takes the load-time linter with it. Second, we
already have an extension mechanism and it is a standard: MCP
(`internal/services/mcpserverservice`, `mcpworker`, `toolsproviderservice`).
Adding a second, non-standard, in-process one is exactly the "vendor-defined
extensibility" pattern V1 deleted on purpose.

### 1d. `ee/blueprints/expr` — one implementation, two hosts — **DON'T** (but the technique is worth remembering)

The best-engineered thing in the monorepo. `expr.js` is the *only*
implementation of the page/v1 expression language; Go runs it inside goja
through a JSON-only bridge (`ee/blueprints/expr/expr.go:36` — everything
crosses as JSON text so goja's Go-value wrappers cannot smuggle in semantics
the browser lacks), and one `fixtures.json` conformance suite is run against
goja by `expr_test.go` and against Node by `conformance.node.js`, pinned to
identical expected outputs. Zero drift between backend and browser by
construction.

We have no second host. Beam is one binary, no browser, no TS surface — the
V1 reshape deleted the web UI and the VS Code extension precisely so there
would be no second host to keep in sync. The technique is filed for the day
one appears; the code has nothing to attach to.

---

## 2. The embed pipe and the index-schema system

The pipeline is real, tested, and Vald-free. It lives entirely under
`ee/bob2/internal/`, ~3,400 LOC non-test with ~2,100 LOC of tests:

`chunker/` 424 · `embed/` 197 · `vectorstore/` 281 · `chunkstore/` 744 ·
`indexconfig/` 204 · `indexservice/` 611 · `indexbridge/` 245 ·
`indexconsumer/` 221 · `searchmodels/` 202 · `ragharness/` 280.

Flow: a VFS write emits an event → `indexconsumer` tails a per-tenant cursor
(5s tick, batch 200, "retry by not advancing") → `indexbridge` gates on MIME,
picks a chunker, chunks → `indexservice.IngestChunks` embeds each chunk
**serially** (`ee/bob2/internal/indexservice/indexrepo.go:94`) with optional
LLM keyword enrichment → vectors to a per-tenant SQLite file, chunk text +
a Postgres generated `tsvector` column to `rag_chunks`. Query side is hybrid:
a vector leg (brute-force cosine) and a lexical leg (`websearch_to_tsquery` +
`ts_rank_cd`) fused by RRF at `k=60` (`indexrepo.go:165`), with an LLM
question-rewrite pass in front. Vald appears nowhere in the data path — the
only two `vdaas` hits in the repo are string constants inside the operator.

### 2a. The whole embed pipeline — **DON'T**

The named problem a port would attach to is agent codebase/document
retrieval. Held against what already exists, that problem is mostly already
answered, and answered better:

- **Go code:** `docs/development/blueprints/gointel.md` claims it — symbol-level,
  type-checked, allow-tier, spike-proven at 19 packages in 3.9s cold, with
  published evidence (MGD, Serena, aider) that symbol-scoped retrieval beats
  file/text retrieval on token cost and hallucinated identifiers. Semantic
  chunk retrieval loses to a type checker on code, and it loses in the way
  that matters — it cannot tell you *who calls this*.
- **Cross-file content search:** `rg` and `grep` are already in the
  safe-shell **allow tier** (`internal/services/hitlservice/policy.go:951`).
  The agent has unapproved ripgrep over the workspace today.
- **Project conventions:** `internal/services/agentsmd/agentsmd.go` loads
  AGENTS.md whole (64 KiB cap) walking up from the workspace root. No
  retrieval needed.
- **The genuinely empty slot** is `contextasm.KindRepoMap`
  (`internal/kernel/contextasm/segments.go:17`) and its `ClassRepoMap`
  retention class — declared, with no producer. gointel's `go_symbols` is
  the planned producer, and it is structural, not semantic.

So the port would buy semantic recall over a query class nobody has named,
and the bill is: an embedding model dependency on the critical path of
indexing (EE's own embedder is cgo llama.cpp behind a `llama_embedder` build
tag — `ee/bob2/internal/embed/bootstrap_llama.go:1` — with a stub returning
`ErrNotBuiltIn` by default, so the "which provider" question is unsolved
there too); a dimension-pinning migration story; a background indexer with a
durable cursor; and a search that is a **full table scan** — `Search` reads
`SELECT id, dim, data FROM vectors` and cosines every row in memory
(`ee/bob2/internal/vectorstore/vectors.go:168`), no ANN index. Honest for a
small corpus, but it is not a retrieval engine, it is a linear scan with a
model in front of it.

The pieces of the pipeline that are unambiguously not ours regardless:
`chunkstore` (Postgres `tsvector`/`websearch_to_tsquery` — we have no
Postgres), `indexconsumer` + `indexbridge` (only meaningful with a
tenant-scoped event log), `searchmodels` (an untested GGUF downloader),
`ragharness` (integration-only, needs Docker + CGO + a HuggingFace
download), and everything multi-tenant.

### 2b. If retrieval is ever wanted: BM25 over the workspace, not embeddings — **RETHINK-SHAPE, gated**

The mine's best file for us is not in the embed pipeline at all. It is
`ee/runner/internal/grounding/grounding.go` — **372 LOC, 213 LOC of tests,
stdlib only** (`math`, `sort`, `strings`, `unicode`, `io/fs`; verified — no
external imports). A BM25-lite in-memory corpus: `Load(dir)` walks `.md`/
`.txt` with `MaxFiles = 200` / `MaxTotalBytes = 5 MiB` caps, `FromDocs` is
the filesystem-free seam for tests, `Retrieve(query, k)` scores at
`k1=1.5, b=0.75` with the always-positive `log(1+…)` IDF form and sorts
**deterministically** (score desc → filename asc → load order). Snippets are
rune-safe, word-boundary-trimmed, centred on the earliest hit with 80
characters of lead-in. A `nil *Corpus` retrieves nothing, so "no corpus
configured" needs no branch at call sites. It never retains a path — only the
base name — so a corpus cannot leak directory layout.

Everything about it fits the house doctrine: deterministic (therefore
golden-testable, which an embedding model is not), local-first by
construction, capped, and honest about being lexical.

- **Target:** `internal/services/grounding` (service, per the standing rule),
  one allow-tier tool `doc_search` in `localtools`.
- **Deliberately dropped:** the `Doc`/evidence-bundle coupling to runner's
  scenario types; directory loading via `os` (walk through `vfs` instead, so
  control-plane-denied paths and out-of-root symlinks never surface — the
  same rule the `@file` picker obeys, `beam-tui.md` §composer); persistence
  (rebuild in memory, the caps make it cheap); any notion of a corpus
  spanning more than the resolved workspace root.
- **Dependency cost: zero.** stdlib only.
- **Gate:** it ships when dogfooding produces a real miss that `rg` did not
  answer. Until then it is a paragraph in this file, which is the correct
  amount of it to own.

### 2c. Bankable, not buildable: the local vector store and the schema discipline

Two things worth recording so that *if* embeddings ever earn their place, the
design is already decided and the cost stays low:

**`ee/bob2/internal/vectorstore/vectors.go`** (281 LOC, `vectors_test.go` 97)
is a file-per-tenant SQLite vector store — float32 BLOBs, exact cosine,
pure-Go `modernc` driver, index treated as a rebuildable derived artifact. It
imports `github.com/contenox/runtime/libdbexec` — which is **our**
`internal/libdbexec`, API-identical: `NewSQLiteDBManager(ctx, path, schema)`,
`WithoutTransaction()`, `WithTransaction(ctx, onRollback...)` all match
(`internal/libdbexec/sqlite.go:24,145,150`). Dropping the `tenant` argument
and changing the import path is very close to the whole port: ~150 lines.
The `onRollback` compensation hook we already have is exactly what
`indexservice` uses to delete inserted vectors when the surrounding
transaction rolls back (`ee/bob2/internal/indexservice/indexservice.go:132`)
— non-transactional vector writes made safe by a registered compensator.

Verified, and it matters for the lexical half: **FTS5 works in
`modernc.org/sqlite v1.45.0`**, which is already our dependency
(`go.mod:26`) — `CREATE VIRTUAL TABLE … USING fts5` plus `MATCH` executes
clean. So EE's Postgres-only lexical leg has a zero-new-dependency
replacement here whenever it is wanted.

**`ee/bob2/internal/indexconfig`** is the most portable *idea* in the mine and
it is not code, it is a discipline. `Config` pins `EmbeddingModel` and
`EmbeddingDim` per pipeline, and the store has **no Update and no Delete**
(`ee/bob2/internal/indexconfig/store.go:30`) — the primary key is
`(tenant, pipeline)` and a re-create is a unique violation. Changing the
embedding model is therefore *by construction* a new pipeline plus a reindex
plus a cutover, never an in-place edit. That is the same instinct as our
manifest-mismatch check in `contextasm` (a prefix that cannot be safely
paired with resident KV is an error, not a silent reuse), and it is the right
default for any derived index we ever build.

Also bankable if a chunker is ever needed: `ee/bob2/internal/chunker/` (424
LOC impl, 423 LOC tests, imports only `context`). Its value is the invariant
`Text == source[Start:End]` on byte offsets, enforced in tests, so every
chunk cites an exact source span; and the markdown chunker's fence-aware ATX
sectioning with a heading-path `Anchor`. Our `ollamatokenizer.CountTokens`
satisfies its `TokenCounter` interface with a two-line adapter, and
`estimatetokenizer.go` is the model-free fallback its `CharCounter` plays.

### 2d. `ee/vald-operator/` — **DON'T**, and EE agrees

1,569 LOC of kubebuilder v4: a `VectorStore` CRD provisioning one Vald
cluster per `(tenant, dimension)`, a phase-pipeline reconciler, finalizer-gated
deletion, envtest + kind e2e. Competently built and **parked by its own
authors**: `ee/docs/operator-crd-draft.md:3` states the model "does not scale
to the 1000+ target" and that the module is "intentionally unwired from
`go.work` and CI", and `ee/docs/vectorstore-sqlite-design.md` records that
the SQLite store supersedes it. A Kubernetes operator in a single-binary
local harness needs no argument beyond stating it.

The one thing worth a sentence: the CRD uses CEL
`self == oldSelf` validations to make `tenant`, `dimension`, and `distance`
immutable after creation — the same create-once discipline as §2c, expressed
in a schema. Cited as corroboration, not as a thing to build.

---

## 3. The open sweep

Everything below is **DON'T**. Recorded so the same ground is not re-walked.

**Retry / backoff / circuit breaking.** There is none in EE — grep for
`backoff|jitter|circuitbreak|retryable|maxRetries` across all Go returns
zero, and `cenkalti/backoff` is only an indirect Helm dependency. We already
have `internal/kernel/taskengine/llmretry/` (258 LOC + 260 LOC tests),
`retry_outcome.go`, and one shared Retry-After-honoring HTTP helper
(`internal/models/modelrepo/httpclient.go`). Nothing to mine, in either
direction.

**Sandboxing / isolation.** Also zero code — grep for
`sandbox|seccomp|landlock|chroot|bubblewrap|rlimit|cgroup|nsjail` across EE's
Go finds only `signal.NotifyContext` and a pagination error. What exists is
prose: `ee/docs/40-own-your-harness.md` §2.6 is a good menu (bubblewrap/nsjail,
`SysProcAttr.Cloneflags` + cgroups v2 + rlimits, seccomp user-notification
routed into an approval broker, Landlock fs/net, wazero). **We have already
built most of it**: `internal/libsandbox/` is 348 KB across landlock
(`landlock_linux.go` 14.8 KB), a netns/tun egress bridge (`netbridge_linux.go`
19.7 KB), a seccomp user-notify tap (`synctap_linux.go` 21 KB), egress policy,
env scrubbing, and exec-path resolution — each with tests. The direction is
reversed: that document is a wish-list for what this repo now ships. The one
item it names that we still lack is rlimits/cgroups — the same gap §1b hits.

**Terminal / TUI / PTY.** Zero in EE. Note for whoever finds it:
`ee/docs/40-own-your-harness.md:227` records "**Decision: do not build a
TUI**" — but read the two lines above it, where the reasoning is that
restoring the *old* TUI would resurrect visualization tied to a deleted DAG
plan engine, and the alternative it endorses is the React Beam that V1 then
deleted. It is a stale verdict against a different artifact, superseded by
`WHY.md` and `beam-tui.md`. Not a live objection.

**Telemetry / tracing / metrics.** EE owns almost none — no OpenTelemetry, no
Prometheus outside the operator's controller-runtime scaffold; everything
defers to our own `libtracker`. The locally-owned pieces are a 53-LOC
request-ID/`traceparent` middleware with no tests, and the
`WithActivityTracker(svc, tracker)` decorator pattern that is already ours.
Nothing to take.

**Eval / judge harnesses.** None exist — no judge, no rubric, no scorecard.
Two adjacent things are worth one line each. `ee/runner/cmd/kvforkbench/main.go`
(534 LOC, no tests) is an honest measurement harness — three strategies at
N ∈ {1,4,8,16}, recording prefill tok/s, decode wall time, peak VRAM, RSS,
with the verdict written up as "partially supported" rather than as a win. The
shape (A/B by configuration, own numbers, honest verdict) is the shape
gointel's §"Expected results and how we will measure" already commits to.
And `ee/blueprints/` carries a golden + determinism + *fixture-coverage*
test discipline (`TestGoldenModelCoversEveryFieldType` asserts the golden
fixture exercises every field type in the vocabulary) that rhymes with our
own contract tests. Patterns, not code.

**The generate → validate → repair loop** (`ee/blueprints/generate/`, 819 LOC
+ 564 LOC tests). Bounded at `DefaultMaxRounds = 3` (one draft, two repairs);
the repair prompt carries the **full diagnostics list as JSON, never prose**;
bounded exhaustion returns a `Result` with a nil error so the caller keeps the
per-round `Trace`; a `Completer` one-method LLM seam with a deterministic
`StubEngine` as the CI default. It is a good design and it is **already our
plan** — gointel V1.1 is precisely "feed structured type errors back in the
same breath as the edit", and `chainlint` + `contenox vet` already do
diagnostics-as-teaching-errors at load time. Cite as corroboration for
gointel V1.1; port nothing.

**Edit-preserving merge** (`ee/blueprints/merge/merge.go`, 387 LOC + 259 LOC
tests, stdlib only). Human edits are declared in-document with a sibling
`"edited": true` that freezes the node *and its subtree*; regeneration matches
by `id` position-independently and records rejected proposals as
`Conflict{Path, Kept, Discarded}` rather than dropping them. Genuinely liftable
— and it solves a problem we do not have. A coding harness has no
regenerate-the-whole-document step; the model edits files directly under
read-before-write and approval, and git is the merge tool.

**Closed diagnostic codes + RFC 6901 pointers**
(`ee/blueprints/validate/errors.go`, 78 LOC, 20 codes). Superseded here by
`internal/errdefs` plus the severity-marker convention in
`internal/services/localtools/hardening.go` — greppable
`(recoverable: …)`/`(fatal: …)` suffixes chosen deliberately as error-string
craft rather than a type system.

**`SplitCommand`** (`ee/runner/internal/config/config.go:227`, ~65 LOC,
tested) — a quote-aware POSIX-ish argv splitter. We deliberately do not want
it. Our policy matcher (`hitlservice/policy.go:637` `commandTokens`) uses
plain `strings.Fields` and is *safe because it fails closed*: shell mode
requested, or any character in `;|&><`\n\r`, or any command-substitution
pattern, and the call can match no allow prefix at all. Parsing quotes more
accurately would widen what the allow tier can match. This is a case where the
graveyard's more sophisticated code is the worse design for us.

**Postgres lease queue** (`ee/bob2/internal/store/source_instances.go:471`) —
a textbook `SELECT … FOR UPDATE SKIP LOCKED` claim-with-lease, ~55 lines,
tested. It answers multi-worker contention over a shared Postgres. We are
single-machine and SQLite-first, and already have
`internal/store/runtimetypes/jobqueue.go` (228 LOC, with `scheduled_for`,
`valid_until`, `retry_count`). EE has no leader election either — `grep leader`
returns zero — so there is not even a coordination mechanism to take.

**Connector abstraction** (`ee/bob2/internal/connector/`, 144 LOC + 197 LOC
tests) — a stateless-connector/central-state document-sync contract
(`Doc`/`Item`/`Source`/`Sink`) whose good idea is that `Checksum` is a
*source-version token* rather than a hash of stored bytes, so transform-on-
ingest stays idempotent. It is a document-sync abstraction, not a tool or
plugin abstraction; MCP is our connector story. The adjacent
`connectorregistry` trick — plugin manifests carried in an OCI image label
(`io.contenox.connector.manifest`) and discovered by scanning a registry for a
name prefix — is genuinely clever and attaches to nothing we do.

**`ee/bob2/tools/openapi-gen/`** (1,591 LOC, no direct tests) — walks the Go
AST to emit an OpenAPI 3 document from `http.ServeMux` registrations and
doc-comment annotations. The V1 reshape deleted our OpenAPI generator and the
HTTP API on purpose. Explicitly out of scope.

**The redaction seam** (`ee/runner/internal/engine/engine.go`,
`Scenario.RedactedForEvidence`) — confidential material reaches the model
prompt while the recorded transcript substitutes a marker and keeps only
filenames. A nice two-half pattern; the closest analogues already exist here
(`internal/libtracker/redact.go` redacts values and keeps field names;
`internal/kernel/taskengine/state_sanitize.go` bounds and summarizes captured
payloads for observability storage while in-memory history keeps originals).
Worth knowing the axis exists — prompt-visible vs transcript-visible is not
quite the same cut as secret-vs-not — but no port.

**Everything in `ee/bob2/internal/{server,store,connectorruntime,apprelease,
appcatalog,auth,vfs*,beamservice}`** — ~30k LOC welded to Helm v3, k8s
client-go/cli-runtime, testcontainers and multi-tenancy. `ee/bob2/go.mod`
carries roughly 200 dependencies. Anything taken from bob2 must be extracted
file-by-file; it can never be imported.

---

## Ranked shortlist (max 5)

1. **`ee/runner/internal/grounding/grounding.go`** — stdlib-only BM25 + snippet
   extraction, 372 LOC, 213 LOC tests, deterministic. The retrieval mechanism
   that actually fits a local-first harness. **Gated**: ships only when
   dogfooding produces a miss `rg` did not answer. Target
   `internal/services/grounding` + one allow-tier `doc_search` tool. Zero
   dependency cost.
2. **`ee/bob2/internal/vectorstore/vectors.go` — banked, not built.** 281 LOC
   that compile against our own `internal/libdbexec` with an import-path
   change and the tenant argument dropped (~150 lines). Recorded here
   precisely so embeddings are *not* pre-built: the future cost is low enough
   that speculative work is unjustified.
3. **The create-once index-schema discipline** (`ee/bob2/internal/indexconfig`).
   Not code — a rule: any derived index pins the model and dimension that
   produced it, and changing either is a new index plus a reindex plus a
   cutover, never an in-place edit. Store with no Update and no Delete.
4. **`ee/bob2/internal/chunker/`** — rides along with 1 or 2 if either ever
   needs splitting. Its value is the `Text == source[Start:End]` byte-offset
   invariant (exact citations) and fence-aware markdown sectioning. Imports
   only `context`; our `ollamatokenizer` satisfies its `TokenCounter`.
5. **A fresh bounded `js_eval`** — a *build*, not a port; the mined code
   contributes nothing. Gated on a dogfooded need, and blocked on the honest
   finding that allow-tier-by-construction fails for memory: goja has no
   memory limit and beam has no rlimit/cgroup anywhere.

## Leave buried

`ee/vald-operator/` (all of it — parked and superseded by EE's own docs) ·
`ee/bob2/internal/embed` (cgo llama.cpp behind a build tag, one provider) ·
`searchmodels` (untested GGUF downloader) · `indexconsumer` + `indexbridge`
(need a tenant-scoped event log) · `chunkstore` (Postgres `tsvector`) ·
`ragharness` (integration-only, Docker + CGO + HuggingFace) ·
`ee/bob2/internal/functionexec` and the historical `ee/bob/jseval` +
`jssandboxhook` (forty reusable lines, two inherited bugs) ·
`ee/blueprints/expr` (no second host to keep in sync) ·
`ee/blueprints/generate` (already our plan, as gointel V1.1) ·
`ee/blueprints/merge` (no regenerate step to merge against) ·
`ee/blueprints/validate/errors.go` (superseded by `errdefs` + severity
markers) · `SplitCommand` (our fail-closed matcher is the better design) ·
the `SKIP LOCKED` lease queue (single-machine, and `jobqueue.go` exists) ·
`ee/bob2/internal/connector*` (document sync, not tools; MCP is our story) ·
`ee/bob2/tools/openapi-gen` (the reshape deleted this surface on purpose) ·
`apiframework/requestid.go` (53 LOC, untested, trivially rewritable) · the
tracker-decorator, phase-pipeline, vendorsync and redaction patterns (worth
knowing, nothing to move) · all of `ee/{apps,business,deploy,hooks,packages,
site,scripts}` · `ee/tmp.md` (a Russian-language YouTube transcript) · and
every EE package under `server`, `store`, `connectorruntime`, `apprelease`,
`appcatalog`, `auth`, `vfs*`, `beamservice`.

## Addendum (same day) — the event engine, deep-read on the maintainer's port question

The maintainer asked whether `ee/bob2`'s event engine — "battle-tested" —
should port, given that taskengine already has an edge-transition system.
A dedicated deep-read (eventdispatch, eventstore/eventsource, functionstore,
functionexec, indexconsumer, plus git archaeology) answered.

**Verdict: DON'T port. Add to leave-buried: `ee/bob2/internal/eventdispatch`,
`functionstore`, `functionservice`, `store/eventmappings.go`,
`server/eventbridge.go`.** The battle-tested claim is refuted by the
repository's own history: two commits, zero fixes, seven weeks untouched, no
API-level test, and the one durable consumer of the event log (indexconsumer)
bypasses the dispatcher entirely. The mechanism: predicate = one
string-equality hash key (taskengine's matcher has six operators); sole
action = tenant JavaScript run SYNCHRONOUSLY on the producer's goroutine
(at-most-once, no delivery record, no vm.Interrupt, no recover() on the
path); cache = unbounded cross-tenant full-table reads every 30s — a
remote-Postgres deployment artifact, not a mechanism. Strip tenancy and the
store types and ~40 lines remain: a map, an RWMutex, a for loop.

**The three-plane ruling this settles:**

1. **In-run reflexes (steer the agent mid-chain) → taskengine, as closed
   extensions to the transition contract.** Both halves ~exist:
   `edge_traversed_at_least` already fires branches on run-state counters
   (tasktype.go), and step-time macro expansion already injects conditional
   steering text every step (step_macros.go; macroenv.go's TOOL PREFERENCE
   paragraph is a shipped conditional reflex). The gap is authoring sugar —
   a counter that isn't an edge (tool calls, tokens) and an
   inject-and-resume shape without a detour task — added to
   handler_signatures.go so chainlint keeps proving branches at load time.
   Discipline precedent to keep: fleetservice hard-caps its one nudge and
   documents the cap so it cannot be "fixed" into a retry loop.
2. **After-the-fact notification (mission events, inbox) → libbus + the
   hand-wired lane table.** Correctly placed today: reportrouter's four
   lanes differ only in what an undeliverable delivery MEANS — a reviewed
   human judgement with a documented never-drop invariant. A mapping table
   would flatten it into a lie.
3. **User-defined automation ("when a mission derails, run X") → the one
   honest job for a general engine, and it stays GATED.** No TODO entry, no
   dogfooded need, and D28 currently answers no to steering running units.
   If the gate opens: build fresh as `internal/services/automationservice` —
   SQLite mapping table beside jobqueue.go (read-on-demand, never polled),
   predicates reusing OperatorTerm, and exactly three bounded V1 actions:
   file an inbox item, enqueue a job (jobqueue's retry_count IS the
   retry/DLQ story bob2 never had), run a named chain under ComputeBounds.
   No goja action ever — that slot belongs to MCP, or to js_eval under §1b's
   gate and blocker.

**Banked:** the two-plane shape bob2 stumbled into — a fire-and-forget
reaction plane plus a cursored catch-up plane over one durable log — is
right, and the runtime already has both halves in better condition (libbus +
the KV task-event journal). Recorded so it is not re-derived; nothing to
port.
