# gointel — in-process Go code intelligence for the agent

Decision record, 2026-07-27. Status: designed, spike-proven, awaiting build.

## Why

The agent reads Go with grep eyes: no "who calls this", no "what type is
this", and no diagnostics after an edit — the loop is edit → shell build →
parse error text. gointel closes all three with an in-process, type-checked
view of the module, exposed as allow-tier tools.

**Why not gopls/LSP:** gopls is pure Go but its implementation is
`internal/` — not importable. Its engineering weight (incremental
re-typechecking, daemon lifecycle, a wire protocol) exists for keystroke
cadence; an agent edits whole files at tool-call cadence, so a warm
snapshot with coarse invalidation is enough. The public
`golang.org/x/tools` API (`go/packages`, `go/types`, `typeutil`,
`go/analysis`) provides everything V1 needs, in-process, no subprocess.
Spike (2026-07-27, scratchpad): 19 beamtui packages loaded and fully
type-checked in 3.9s cold; definition, cross-package references (18 hits for
`frame.StyleBrand`), hover-grade types, and zero-build diagnostics in ~60
lines. One honest dependency: `go/packages` drives `go list`, which any
buildable Go repo already requires.

LSP-as-subprocess remains the explicitly deferred path for languages with
no importable type-checker (TypeScript, Python, Rust). Same future tool
vocabulary, different backend. Not V1.

## Prior art — what proven systems measured

The design is funneled through published results from non-Go ecosystems;
each gointel feature traces to evidence:

| System | What it proved | Number | gointel feature it backs |
| --- | --- | --- | --- |
| **SWE-agent** (NeurIPS '24, Princeton) | Tool-interface design ALONE moves outcomes: purpose-built structured tools with concise uniform feedback vs raw Linux shell | +10.7 pp on SWE-bench Lite over shell-only; 12.47% vs prior best 3.8% full-set | Building first-class tools with capped, file:line-anchored, deterministic output — not "the agent can shell out to tooling" |
| **Monitor-Guided Decoding** (NeurIPS '23, Microsoft, via multilspy) | LSP-derived static context (type-directed completions) kills hallucinated symbols | 20–25% compilation-rate improvement across 350M–175B models; CodeGen-350M+MGD beat text-davinci-003 on compilability | `go_describe`/`go_definition`/`go_references` — real symbol truth in context instead of guessed APIs |
| **RustAssistant** (ICSE '25, Microsoft) | Iterating with structured compiler errors fixes most compile failures | ~74% auto-fix on real-world repo compilation errors (93% micro-benchmarks) | The post-edit diagnostics loop (V1.1): feed type errors in the same breath as the edit |
| **aider repo map** (2023, tree-sitter/ctags) | Symbol-outline context lifts editing performance on its editing benchmark; earliest public evidence for structural context | benchmark deltas documented in aider's ctags/repomap posts | `go_symbols` outlines as cheap, high-value context |
| **Serena** (oraios, MCP + multilspy) | Symbol-level retrieval/editing tools (find_symbol, find_referencing_symbols) are dramatically more token-efficient than file reading; widely adopted with editor-grade navigation for agents | qualitative/adoption evidence; token-efficiency is its core claim | Names-first tool vocabulary; symbol-scoped answers instead of whole files |

Gap gointel fills that none of the above did: every proven system wraps a
language server subprocess (multilspy, Serena) or stays lexical
(tree-sitter). Go is the language whose compiler internals are a public
library — in-process is cheaper (no lifecycle, no protocol, no skew
between client and server) and unlocks the one thing subprocess wrappers
can't do well: **overlay pre-flight** — type-checking a PROPOSED edit
before it is applied.

## Decisions

- **Names-first queries.** LSP is position-oriented because editors have
  cursors; agents have names. Tools take qualified symbols
  (`pkg.Ident`, `pkg.Type.Method`); file:line only disambiguates.
- **Snapshot model, honestly coarse.** One immutable snapshot per module
  root (`packages.Load ./...`, full syntax+types); queries read lock-free.
  Any agent write to `.go`/`go.mod` marks dirty; next query rebuilds (edit
  bursts coalesce). External edits caught by mtime sweep at query time. NO
  package-granular incrementality in V1 — that is gopls's 80% and agent
  cadence does not need it.
- **Bounded like shellsession:** lazy build, LRU of 2 module roots,
  15-minute idle reaper, closed on engine stop.
- **Advisory, never blocking.** x/tools tracks Go releases; a repo on a
  newer toolchain may see phantom errors. Diagnostics are strong signal;
  `go build` stays the arbiter. Every result names its toolchain view.
- **No rename in V1.** Conflict-safe rename is where gopls's internals earn
  their weight; a wrong rename destroys trust. Later, approve-tier, or never.
- **Build context defaults stated in tool descriptions**: host GOOS/GOARCH,
  no tags, tests excluded by default (memory).

## Tool surface (all allow-tier — pure reads)

`go_describe` (type, signature, doc, fields/methods) · `go_definition` ·
`go_references` (grouped by file, snippets, capped "+N more") ·
`go_implementations` (both directions) · `go_symbols` (outline) ·
`go_diagnostics` (type errors + curated vet passes; scope changed|package|all).

## The loops (phased — they touch kernel and HITL)

- **V1**: toolset only.
- **V1.1 post-edit verification**: engine middleware appends fresh
  diagnostics for touched files to every successful write-class tool result
  on `.go` files. (RustAssistant is the evidence this loop pays.)
- **V1.2 pre-flight**: approval cards for `.go` diffs carry "compiles clean /
  introduces N type errors" via a single-package overlay check (export-data
  deps; sub-second warm). Extends the envelope from bounded actions to
  verified outcomes.

## Expected results and how we will measure

Priors justify the direction; they do not grant us their percentages. Own
measurement before/after, A/B by omitting tool registration:

1. **Hallucinated-identifier rate**: seeded tasks referencing real repo
   APIs; count `undefined:`-class build failures per task (MGD predicts a
   large drop).
2. **Edit-to-green cycles**: seeded type-error fix tasks; count
   build/tool-call round trips until green (RustAssistant predicts most
   errors fixed in one fed-back iteration).
3. **Localization token cost**: "find all callers of X and update"
   tasks; tokens consumed to first correct localization (Serena/aider
   predict symbol-scoped answers beat file reading).
4. **Task success on a fixed script** of repo-realistic tasks (SWE-agent
   predicts the tool shape itself lifts success).

## Risks

Version skew (x/tools vs repo toolchain) — advisory framing + named
toolchain per result. Memory (hundreds of MB warm) — LRU + reaper.
Cold-load seconds on big repos — lazy + cache. Stale-snapshot answers —
the one way this design lies; invalidation logic gets the adversarial
review pass.

## Build order

1. loader/snapshot/invalidation (+ fixture module tests)
2. queries + diagnostics + toolset registration (+ envelope rules: all allow)
3. adversarial review on invalidation; measurement harness (§Expected results)
4. V1.1 engine middleware (service extension, own review)
5. V1.2 approval-card pre-flight
