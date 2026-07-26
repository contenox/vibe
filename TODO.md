# TODO — contenox (post-V1-reshape)

**Direction:** an open coding harness — terminal CLI + ACP editors + the beam
TUI (in development), thin surfaces over the kernel (the Dagger-like heart;
WHY.md). **Envelopes ARE contenox** (maintainer, 2026-07-26): without
policy-bounded execution it's something else — slices that make envelopes
more real (durable, enforced, verified) outrank everything cosmetic.
Standing rules: build on services, never in surfaces · mine mechanisms,
never shapes (pando = anti-model; dual-evaluate replaceables like eino) ·
everything surfaced must actually work.

**SHIPPED 2026-07-26** (commits `b81624f3`+`c06751f9`, history purged
197→13MB, force-pushed, repo renamed contenox/beam): V1 product purge (web
UI, HTTP API+apiframework, spec generator, modeld daemon+client, VS Code
ext, UI lib, REST-lever verbs, 5 dead libs), layered restructure
(`internal/{kernel,models,services,store,surfaces}`, module
`github.com/contenox/beam`), Make→Task, harness repositioning (copy EN+DE,
Retired-R&D page, media on S3), WHY.md, beam blueprint + Crush/pando mining
specs + eino decision record (LEARN, `eino-evaluation.md`), vision plumbed
end-to-end (ACP image blocks + `chat --attach`), `beam` teaching stub.

## 1. IMPLEMENTATION SLICES (ordered; each committable with a named gate)

### S1 — Streaming truth (eino T1+T2+T7 · audit C1, C2, I3, I9) — M
The structural fix, not the patch. Providers emit raw deltas only
(`{ContentDelta, ThinkingDelta, ToolCallDelta{Index,ID,Name,ArgsFragment},
Usage, ErrorEvent}`); ONE engine-side assembler (index-grouped, atomic-field
consistency, hard errors wrapped with provider context); typed terminal
parcel (usage + finish reason — length/content_filter no longer ignored);
`Enabled()` → per-kind `Wants(kind)` so observation can never fork execution
semantics; in-stream error events surfaced (anthropic SSE `error`, cc-codec
error payloads); ctx.Done() on every relay send (llmrepo leak); gemini/vertex
scanner limits raised; **C2** gemini stream system-prompt fix rides along.
Per-provider golden SSE fixtures replace the mock-only event test.
*Gate:* streamed tool-calling works on all 9 providers against fixtures;
`grep`-able proof the old per-provider assembly paths are deleted.

### S2 — Capability truth (audit C3, C4 + vision surfacing) — S/M
Stop CanEmbed lies (mistral/openrouter/vertex: implement or stop
advertising); merge observed CanVision/CanThink into the declared-model
branch (additive, like ollama); verify anthropic catalog caps against the
live API; surface CanVision in `model list` + doctor so a failed vision
route teaches. *Gate:* declared gpt-4o/claude routes vision; embed on
mistral either works or refuses at the catalog, never at the connection.

### S3 — Provider correctness round 2 (audit I1, I2, I4, I5, I6, I8, I10) — M
- [ ] I5 typed sentinels first (ErrContextLengthExceeded/ErrRateLimited
      mapped per provider) — context-budget recovery depends on it
- [ ] I2 OpenAI Responses reasoning from output items (+ request it;
      store:false) · I4 anthropic thinking round-trip via ProviderMeta
- [ ] I1 vllm auth key · I8 mistral random_seed + ContextLength · I10
      timeouts everywhere + minimal 429/5xx retry with Retry-After
- [ ] I6 bedrock: inference-profile IDs, Think decision (support or refuse
      loudly), document history-image drop
*Gate:* parity matrix re-run shows no silent lies; NICE list triaged.

### S4 — Chain & envelope vet (eino T3) — M
Handler signature registry (closed table) + DataType dataflow walk over
goto/on_failure edges (eino tri-state: impossible=load error, maybe=runtime
backstop) + input_var/macro reference checks + teaching errors naming both
endpoints + sticky disable on the stored row. One `contenox vet` verb
covering CHAINS and ENVELOPES (hitl-policy files are identity — they get
load-time validation and the same teaching voice). Wired into
taskchainservice (write+read), chainagents.Discover, ExecEnv backstop.
*Gate:* the runtime SEVERBUG class unreachable in tests; a broken chain or
envelope teaches at load, never mid-run.

### S5 — Engine event contract (eino T5) — S
Terminal `step_stream_end` event (chunk count + usage); hierarchical scope
addresses (chain/task[/toolCall]) on events + captured state; per-kind field
matrix documented as THE engine-bridge contract (asserted against the
acpsvc translator in a test). Prerequisite for S6 and for beam.
*Gate:* replay carries stream brackets; matrix doc = translator behavior.

### S6 — Durable envelopes (eino T4) — L — **the identity slice**
Interrupt/checkpoint HITL: HITLWrapper third outcome `ErrApprovalPending`;
JSON checkpoint {vars+types, edgeCounts, history, pendingToolCalls} keyed by
approvalID with S5 addresses, versioned envelope + migration hook + field
round-trip tests from day one (eino's gob scar); resume re-enters via the
existing tool-pairing repair path; hybrid policy: park ≤30s fast path,
checkpoint-and-release slow path; nativeturn gains `suspended`; missions
detach on `mission_ask_attention` instead of pinning the executor.
*Gate:* kill -9 mid-pending-approval → restart → respond → chain completes.

### S7 — Envelope enforcement at fleet width (pando F1-G1 + F1-G4 + G6) — S
Admission cap in fleetservice (count open units, teaching refusal naming cap
and value; operator retry bypasses, automatic dispatch never; NEVER expose
the knob before it's enforced) · conclusion verification gate in missiontools
(stat claimed artifact paths; positively-missing downgrades success→partial
with a warning, nothing discarded) · nil-safe fleet counters (dispatches,
refusals, downgrades) surfaced in doctor/mission-panel.
*Gate:* a mission loop cannot exceed the cap; a hallucinated "done, see
file" arrives downgraded.

### S8 — Context prevention (pando F2-G1 filter engine) — M
Tool-output filter engine in localtools per `pando-mining.md`: structured
parsers (go test -json, lint, tsc) with 3-tier degradation → declarative
filters (first-match, project-local overrides outrank built-ins) → fixed
transform pipeline; stderr/exit code structurally untouchable; filtered
inline + RAW ALWAYS IN SPOOL with a notice naming filter and spool path;
`filter test` validator incl. match-assertions; measured savings via tracker.
*Gate:* wedge-class transcript (verbose test run) measurably compressed with
zero error lines lost; kill switch works.

### S9 — Chain format v2 (+ eino T6 call_chain) — design M, impl M
The maintainer ruling: current DSL is not a contract — only "meaningful
chains, okayish to author." Design doc first (user reviews): terse, pleasant
authoring for the whole file family (agents, ENVELOPES, chains); declared IO
signatures; S4 linting and S5 addresses as birthrights; primitives kept
close to eino node/branch/interrupt shapes (keeps the eino revisit trigger
in `eino-evaluation.md` a compiler weekend). `call_chain` handler lands here
(child signature = node signature; include-cycle check at load; ACP
self-spawn stays the deliberate isolation tier).

### S10 — Beam service extensions (blueprint §4) — parallel after S5
libacp max_tokens stop-reason fix (flagged highest-leverage) ·
buildInProcessFleet extraction out of acp_cmd (surface→service) ·
chatservice TrimHistory · remaining blueprint extension list.
Then beam implementation per blueprint build order (test-harness,
theme-styles, engine-bridge, keymap-registry, liveness first) — gated on the
D1–D61 rulings (§3).

Interleave freely: S7/S8 are independent of S1–S3. S6 needs S5. S9 design
can start anytime; its implementation wants S4.

## 2. Remaining vision items
- [ ] Provider-wire e2e (mock) + gated real-model vision test (ollama
      container) — natural rider on S1's fixture harness
- [ ] beam composer paste path (rides beam build; spec in blueprint)

## 2b. Copy & positioning follow-ups
- [ ] **Foreground ENVELOPES in the marketing copy + README** (maintainer:
      envelopes are what makes contenox contenox). Today's copy leads with
      missions and "every rule a readable file"; the envelope appears as
      supporting cast ("an envelope you wrote"). Elevate it to the headline
      layer of landing (EN+DE) and README — the envelope is the named
      concept competitors don't have; missions are what envelopes make safe.
- [ ] Landing: replace the old-way/contenox table with a three-frame
      terminal strip (mission fired → detach → envelope interrupt; commands
      as the only captions) once beam exists to record

## 3. Open decisions (user)
- [ ] **Blueprint D1–D61** — gates beam implementation start
- [ ] **modelregistry fate** — still modeld-era (llama/openvino, MMProjURL),
      consumer (`model pull`) gone: delete or repurpose as hosted catalog?
- [ ] D28 (mission re-entry/await) — pando F1-G2/G3 designs are the answer
      material (budgeted re-entry + await/join); if ruled yes → new slice
      after S6/S7
- [ ] Active-blueprints cull ("99% can go" hinted; mining specs + decision
      records should stay)
- [ ] Orphan chop-list: internal/tooleval (+fixtures; Taskfile uses it),
      scripts/demos/, contenox-agentic-bench.ps1. KEEP: tools/acp-validator,
      libacp/cmd/acp-stub-agent, tools/version
- [ ] Fold statetype INTO runtimestate (deferred package merge)
- [ ] Dependabot: 67 findings (8 critical) — triage (`cd website && npm
      audit` first) · AWS: decommission contenox-modeld-artifacts bucket? ·
      install.sh e2e after next release · demo .tape paths on re-record

## 4. Stale-hunt & hygiene (follow-up sessions)
- `grep -rniE 'modeld|openvino|llamacpp' --include='*.go' .` (known:
  modelregistry §3; acpsvc config_options.go:434 + prompt.go comments)
- `grep -rniE 'contenox/runtime' .` (legacy module path; demo tapes known)
- `grep -rniE 'beam' docs/ website/ README.md` (must mean the TUI or
  Retired-R&D) · `grep -rn 'make ' docs/ CONTRIBUTING.md README.md .github/`
- go.mod: charmbracelet arrives only with beam; grpc stays indirect-only
- Load-flaky tests (pass isolated; harden or serialize): runtimetypes
  TestUnit_Backend_DeletesSuccessfully (Postgres testcontainer — why does
  the SQLite-era store test against Postgres at all?), acpsvc
  TestE2E_Wire_ExternalAgent_CommandMenuAfterNewSessionResult,
  TestLoopback_ExternalAgent_LazyRespawnPushesConfigOptions
- examples/ + `.contenox/` chains re-verified · SUPPORT.md/.github issue
  templates for killed surfaces
- Audit NICE list: llmrepo modeld doc comment · messages-codec vertex mode
  unwired · vllm tool names unsanitized · OpenAI Responses store:false (S3)
