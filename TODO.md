# TODO — contenox

**Direction:** contenox is an open coding harness — a terminal CLI, ACP
editor integrations, and the beam TUI (next up) as thin surfaces over the
kernel (see WHY.md). **Envelopes are the identity**: the approval-policy
files that bound what an agent may do unattended are what make contenox
contenox — work that makes envelopes more real (surviving restarts, enforced
limits, verified results) outranks cosmetic work.

Standing rules: business logic lives in services, surfaces stay thin · from
other projects we take individual mechanisms, never their product shape ·
anything the CLI/docs advertise must actually work.

## Shipped ledger

**2026-07-26 — the V1 reshape** (commits `b81624f3`+`c06751f9`, history
purged of accidental binaries 197→13MB, force-pushed, repo renamed
contenox/beam): killed the web UI, HTTP API layer + framework, OpenAPI
generator, modeld daemon + client, VS Code extension, UI library,
server-dependent CLI verbs, five unused libraries · layered restructure
(`internal/{kernel,models,services,store,surfaces}`, module
`github.com/contenox/beam`) · Make→Task · harness repositioning (copy EN+DE,
one Retired-R&D docs page, media on S3) · WHY.md · beam TUI component
blueprint + Crush/pando whiteroom mining reports + the eino decision record
(LEARN; revisit trigger written down) · image input end-to-end (ACP image
blocks, `chat --attach`) · `beam` reserved as a teaching stub.

**2026-07-27 — phases A–C** (commits `d0ec692a`+`7ed3f252`; providers
trimmed to ollama, vllm, openai, anthropic (kept but expensive — no paid
tests), gemini, bedrock, vertex):
- **Streaming truth**: providers emit raw deltas only; ONE engine-side
  assembler; typed terminal parcels (finish reason + usage); per-kind event
  subscription so observing can never fork execution; gemini stream
  system-prompt fix; in-stream errors surfaced; golden wire fixtures for all
  7 providers.
- **Capability truth**: declared models keep observed vision/thinking;
  vertex embeddings implemented; bedrock's false CanEmbed removed; doctor
  shows a vision summary; pinning a non-vision model with an image attached
  now teach-refuses instead of silently swapping models.
- **Provider correctness**: typed context-overflow/rate-limit sentinels per
  provider; OpenAI Responses reasoning parsed from the right field (+
  store:false); anthropic thinking blocks round-trip so thinking+tools
  multi-turn stops 400ing; vllm auth token flows; one shared HTTP client
  with timeouts + one Retry-After-honoring retry helper; bedrock
  inference-profile resolution + Think support; vllm tool names sanitized.
- **Cache utilization** (design: `provider-kv-cache.md`): session-sticky
  model/backend resolution; prefix determinism (day-granular {{now}} in
  system prompts, canonical tool order, chunked history trim — which also
  fixed a real bug: the old trim silently dropped the AGENTS.md message
  forever); cache-aware usage extraction with a strict normalization rule;
  anthropic cache_control breakpoints (with the API-required thinking-block
  skip), bedrock cachePoints, openai prompt_cache_key, vllm provenance
  wire-leak sealed.
- **Chain & envelope vet**: load-time validation with teaching errors for
  chains AND envelope files; `contenox vet`; sticky disable on broken rows;
  caught a genuinely stale file in the maintainer's own ~/.contenox.
- **Engine event contract**: `docs/development/engine-events.md` is
  normative, with drift-failing contract tests; journaled stream-end
  brackets; hierarchical addresses (chain/task/tool-call) on events and
  captured state.
- **Durable envelopes (the identity slice)**: approvals park ≤30s then
  checkpoint-and-release; versioned checkpoint with migration engine and a
  reflection guard against silent field loss; claim CAS picks exactly one
  resumer; PROVEN: suspend in process A, kill it, respond in process B →
  chain completes exactly once (deny variant too).
- **Mission surfaces**: `contenox mission list/show/reports/fire --wait`
  (in-process, honest semantics, real 5s e2e); fleet composition extracted
  from the CLI surface into `fleetservice.BuildInProcess`.
- **Fleet guards**: admission cap enforced-from-birth (`fleet-max-parallel`,
  teaching refusal); conclusion verification gate (hallucinated "done, see
  file X" arrives downgraded with a warning); nil-safe counters in doctor.
- **Context prevention**: tool-output filter engine (structured parsers for
  go test/lint/tsc, declarative fallback, stderr/exit untouchable, raw
  always in spool; measured: a 95KB go-test transcript → 430 bytes inline
  with every failure intact); `contenox tools filter test` validator.
- **Vision proven live**: a real image through `--attach` → capability
  routing picked the vision model → "I see a red circle."
- **Brand**: beam gold (light-beam amber ramp) across website tokens, CTAs,
  sidebar, and a scheme-aware SVG favicon (fixes the washed-out light-tab
  case); TUI accent ladder + usage rules recorded in the blueprint.
- **libbus**: Request timeout classification made position-independent
  (deadline expiring before/during insert now labels as ErrRequestTimeout)
  — was the full suite's only failure, a pre-existing gap. [staged, rides
  the next commit]

**2026-07-27 — the V1 chop (pre-beam cleanup)** [staged]:
- **Orphans deleted**: internal/tooleval (1,853 LOC + 23 scenario files, zero
  production importers) + its Taskfile task; scripts/demos/ (documented
  retired surfaces, pre-rename tape paths — clears the contenox/runtime grep);
  contenox-agentic-bench.ps1 + companion chain (drove retired modeld);
  internal/services/archiveutil (bonus find during the modeld grep: both its
  callers died in the reshape, zero importers).
- **Blueprint cull**: killed v1-feature-map, local-coding-node-goals,
  product-surface-truth (rule lives in this file's standing rules),
  agent-servers-and-client-e2e, windows/ (live content folded into
  windows-development.md). tool-hardening.md KEPT after the dangling-ref grep
  proved localtools code/comments cite its Rec 4/5/7 vocabulary — reclassified
  as decision record with a retirement note on the eval-harness section.
  acp-client-engine.md KEPT (client-hub capability, not superseded by
  engine-events.md). Indexes updated; agent-sandbox.md added to the acp index
  it was missing from.
- **statetype → runtimestate merge**: single ~100-line file, 14 import sites
  across 5 packages, zero symbol collisions; split had served the dead HTTP
  API.
- **Test truth**: runtimetypes.SetupStore (and the acpsvc/messagestore test
  setups the first sizing missed) now run SQLite — the engine the product
  ships — instead of a postgres:17 testcontainer; the Postgres manager,
  schema.sql, and localpostgres.go deleted (zero non-test consumers);
  suite went from container-bound to 16s. TestSystem_Ollama* short-gated.
  The two acpsvc flaky-under-load tests diagnosed: fixed 3s/5s notification
  drains racing spawned-subprocess startup — deadlines raised to 30s.
- **Hygiene**: modeld/openvino/llamacpp comment mentions fixed (acpsvc,
  llmrepo doc-comment, taskengine, runtimestate; cli.go's reserved-verb list
  stays on purpose); make→task in acp-client.md + windows-development.md;
  SUPPORT.md/issue templates verified already clean; full -short suite green.

**2026-07-27 — envelope core (the identity slice, part 2)** [staged]:
- **Resume hook registered at the composition roots**: acp_cmd injects its
  one durable-ask service into the engine (`enginesvc.Config.HITLService` —
  collapsing the old two-instance split that couldn't wake each other's
  waiters) and registers `agentservice.ResumeHook`; BuildEngine (chat/run/
  doctor/approvals) builds + injects + registers the same way. The
  process-independent resume entry now has production callers.
- **`contenox approvals list/respond`**: the durable ask inbox. `list` runs
  SweepExpired as its production tick (it had zero callers) and renders
  permission asks and questions in one table; `respond`
  (--approve/--deny/--answer) records the verdict and RESUMES the suspended
  run synchronously in this process, with teaching errors for every sentinel
  and honest degradation when no engine can be built. BuildEngine registers
  durable-only missiontools so a resumed MISSION chain's later
  report/finish calls land instead of failing as unknown tools.
- **`contenox mission stop`**: service-first `fleetservice.StopMission` —
  guarded abandon transition, new `hitlservice.AbandonMissionAsks` (closes
  the mission's pending asks hook-LESS; nothing of a stopped mission may
  resume), checkpoints deleted; live teardown via a StatusChanged bus
  subscriber in BuildInProcess (the subject had zero subscribers) so a stop
  from any terminal reaps the unit wherever it runs.
- **Dual-inbox wart fixed**: `RequestApproval` now adopts a caller-supplied
  ToolCallID as the durable row identity (adopt pending / return terminal
  verdict / create-with-ID + race re-read) — one row per ask, never a twin.
  Bonus: the wait now polls the durable row like the attention path, so a
  cross-process respond ends a parked wait instead of hanging to the ceiling.
- **Mission attention detach (questions now checkpoint)**: suspendable-call
  marker at the one execution site whose output satisfies suspendRun;
  `WithAttentionAnswers` (TEXT twin of approval verdicts); AttentionRequest
  gains AskID + ParkWindow with a typed pending error; execAskAttention
  consumes injected answers on resume, parks 30s then releases;
  Answer/AnswerAsAgent got the same waiterless resume-hook treatment as
  Respond; ResumeFromCheckpoint injects text for questions and restores the
  MISSION BINDING from the ask row's attribution (found live: without it the
  resumed chain couldn't see the mission tools at all). PROVEN by
  attention_resume_e2e: ask in process A, kill it, answer in process B → the
  operator's words are the tool result exactly once; refuse variant files
  the durable blocker.
- **Max-tokens truth**: `FinishReason` now rides ChatResult →
  ChatHistory → CapturedStateUnit → InferStopReason, populated by all 7
  providers (openai chat + Responses incomplete_details, anthropic
  stop_reason, gemini/vertex candidates, bedrock StopReason, vllm, ollama) —
  and vllm/ollama non-streaming "length" became a truncated SUCCESS
  (content preserved) matching the streaming assembler's contract instead
  of discarding the partial response behind an opaque error. A truncated
  success now reaches clients as StopMaxTokens; the beam TUI can warn.

**2026-07-27 — deferrals cleared + envelope copy** [staged]:
- **Cache deferrals landed**: StableHistoryLen now produced at the one
  request-build site in taskexec (all-but-last-message asserted stable; a
  trim this call asserts nothing — expected-cold per the design), so
  anthropic BP3 history breakpoints finally fire; ollama keep_alive set on
  every model-loading request (chat/stream/generate/embed; default 10m,
  `CONTENOX_OLLAMA_KEEP_ALIVE` overrides) — one un-annotated path would
  have silently reset residency to the 5m server default; PromptExecute now
  reports provider usage (interface widened through all 7 providers,
  Meta.Usage, TokenUsage event at the prompt call site). gemini explicit
  cachedContents stays deferred behind its own usage-evidence gate.
- **Vision follow-ups**: audit found all 7 providers already carry image
  wire tests (the mock e2e half was done); added the cost-gated real-model
  test `TestSystem_Ollama_Vision` (container + moondream, red-circle PNG,
  answer must contain "red"). Remaining manual: the ACP-editor-side sanity
  check; composer paste rides the TUI.
- **Envelope copy pass**: landing EN+DE positioning now names the envelope
  as the concept ("built around the envelope: the file that bounds what an
  agent may do unattended; missions are what envelopes make safe"), first
  capability card states the shipped fact (envelopes survive restarts —
  checkpoint, answer from any terminal, resumes exactly once); README intro
  reshaped the same way incl. `contenox approvals respond`. German mirrors
  EN; banned-phrase grep clean; website builds.

## 1. Next implementation bites (no decisions needed)

- [ ] **Envelope-core follow-ups** (small, from the shipped slice): the
      spawned-binary fleet e2e for `mission stop` live teardown (service +
      subscriber are unit/e2e-tested in-process; the full two-binary variant
      remains); a CLI-binary-level e2e for `approvals respond` (the composed
      service path is proven by resume_e2e + attention_resume_e2e).
- [ ] **Remaining deferrals** (parked with named gates): gemini explicit
      cachedContents (only if usage evidence demands); the ACP-editor-side
      half of the vision sanity check (manual, needs a live editor); beam
      composer paste path (rides the TUI build).
- [ ] Landing: replace the comparison table with a three-frame terminal
      recording (mission fired → detach → envelope interrupt) once beam
      exists to record it.

## 2. beam TUI (gated on the D-pass)

- [ ] **The D-pass**: maintainer rules on the blueprint's remaining open
      decisions. Much smaller than the original 61 — six constitutional
      rulings already made (in `beam-tui.md` §1: testability doctrine,
      copy/paste constitutional → inline rendering + zero mouse capture,
      the scope bar = chat + four lacks, chat's superpowers are the floor
      incl. Ctrl+E editor handoff at MVP, full ACP command parity by
      construction, missions first-class inline, beam-gold accent rules).
      Claude preps it as a recommended-defaults ruling document on request.
      (No service extensions remain before TUI code: max-tokens truth — the
      last one flagged — shipped 2026-07-27; the TUI can warn about
      truncated output from day one.)
- [ ] Then implementation in blueprint build order — test-harness and
      theme-styles first, everything a pure function of (state, width),
      dogfooded on this repo from the first day it can chat. Whiteroom
      inputs ready: `beam-tui-crush-mining.md` (implementers use the report,
      never the Crush repo — FSL), `pando-mining.md`.

## 3. Open decisions (maintainer)

- [ ] The D-pass (§2)
- [ ] Mission re-entry (blueprint D28): may a finished mission wake its
      supervising agent? pando report §F1-G2/G3 holds the disciplined
      design (budgeted re-entries, await-all/any). If yes → slice it.
- [ ] Dependabot: 67 findings (8 critical) — likely the website npm tree
      (`cd website && npm audit`)
- [ ] AWS: decommission contenox-modeld-artifacts-573643652148?
- [ ] After next release: install.sh e2e against contenox/beam assets.
      Demo recordings start from scratch when needed — scripts/demos/ was
      deleted in the chop (it documented retired surfaces), and the landing
      recording waits for beam anyway (§1).

## 4. Stale-hunt & hygiene (follow-up sessions)

The 2026-07-27 chop ran the full grep list — modeld/make/contenox-runtime/
beam hits fixed or justified, flaky trio + ollama gating fixed, SUPPORT.md
and issue templates verified clean (see ledger). Still standing:
- Re-run the greps before release as a regression check (`modeld|openvino|
  llamacpp`, `mistral|openrouter`, `contenox/runtime`, `beam`, `make `)
- go.mod: charmbracelet libs arrive only with beam code; grpc stays
  indirect-only
- examples/ and .contenox/ chains re-verified against surviving providers;
  maintainer's home dir: `contenox init --update` refreshes the stale
  agent-planner.json that vet caught
