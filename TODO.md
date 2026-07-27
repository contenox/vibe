# TODO — contenox (post-V1-reshape)

**Direction:** contenox is an open coding harness — a terminal CLI, ACP
editor integrations, and the beam TUI (in development) as thin surfaces over
the kernel (see WHY.md). **Envelopes are the identity**: the approval-policy
files that bound what an agent may do unattended are what make contenox
contenox — work that makes envelopes more real (surviving restarts, enforced
limits, verified results) outranks cosmetic work.

Standing rules: business logic lives in services, surfaces stay thin ·
from other projects we take individual mechanisms, never their product shape
· anything the CLI/docs advertise must actually work.

**SHIPPED 2026-07-26** (commits `b81624f3`+`c06751f9`, git history purged of
accidental binaries 197→13MB, force-pushed, repo renamed contenox/beam):
removed the web UI, the HTTP API layer and its framework, the OpenAPI
generator, the modeld local-inference daemon and its client, the VS Code
extension, the UI library, the server-dependent CLI commands, and five
unused libraries · restructured into `internal/{kernel,models,services,
store,surfaces}` with module path `github.com/contenox/beam` · replaced
Makefiles with Taskfile · repositioned all copy around the harness idea
(EN+DE), moved website media to S3, wrote WHY.md · produced the beam TUI
component blueprint plus concept-mining reports from Crush and pando and the
decision record on the eino framework · made image input work end-to-end
(ACP image blocks and `contenox chat --attach`) · reserved `beam` as a
teaching stub so it can't fall through to chat.

**Provider roster (decided 2026-07-27):** mistral and openrouter are REMOVED
(they cost money to test and add no strategic value). Survivors: ollama,
vllm, openai, anthropic (kept, but expensive — no paid tests by default),
gemini, bedrock, vertex.

## 1. IMPLEMENTATION SLICES (ordered; each committable with a named gate)

### S1 — Streaming truth — DONE 2026-07-27
Problem (from the 2026-07-26 provider audit): when output is streamed, 7 of
9 providers silently lose the model's tool calls — each provider implemented
its own delta-assembly, most incompletely, and the engine even runs a
different code path depending on whether a UI is listening. Fix
structurally, the way the eino framework does it (decision record:
`docs/development/blueprints/eino-evaluation.md`):
- providers emit only raw stream deltas (text, thinking, tool-call
  fragments, usage, errors) — no assembly inside providers;
- ONE engine-side assembler merges tool-call fragments by index, with hard,
  provider-labeled errors on inconsistencies;
- an explicit typed "stream finished" parcel carries finish reason + token
  usage (today a finish reason like "length" is silently ignored);
- the event sink interface changes from a single on/off switch to per-event-
  kind subscription, so observing a run can never change how it executes;
- also fixed in the same slice: the Gemini streaming path currently drops
  the system prompt entirely (the non-streaming path handles it correctly);
  provider error events mid-stream are surfaced instead of swallowed;
  stream buffer limits raised (large tool arguments currently kill
  Gemini/Vertex streams); every stream-forwarding goroutine selects on
  context cancellation so an abandoned consumer can't leak goroutines.
- Test strategy: recorded per-provider wire transcripts ("golden fixtures")
  driven through the real adapters, replacing today's mock-only test.
- The mistral/openrouter removal happens first inside this slice.
*Gate:* streamed tool-calling proven by fixtures on all 7 surviving
providers; the old per-provider assembly code is deleted.

### S1b — Provider cache utilization — LANDED 2026-07-27 (waves 1+2)
Done: session-sticky resolution (StickyOrRandom, rendezvous-hashed), prefix
determinism (day-granular {{now}} in system prompts, canonical tool order,
chunked history trim that also fixed the AGENTS.md-dropped-forever bug),
cache-aware usage extraction with a strict normalization rule (PromptTokens
= total on every provider; anthropic/bedrock sum their uncached remainder),
and wire placement: anthropic cache_control breakpoints (with the required
thinking-block skip), bedrock cachePoints (claude/nova-gated), openai
prompt_cache_key from the session key, the vllm provenance wire-leak fixed
with byte-stability tests, gemini/vertex implicit (documented).
DEFERRED (recorded): taskexec rich history hints + trim-shift half (was S6
agent territory); gemini explicit cachedContents (evidence-gated); ollama
keep_alive config; PromptExecute path reports no usage.
Design doc landed: `docs/development/blueprints/provider-kv-cache.md`
(cite-checked provider facts, the recovered modeld stable-prefix doctrine,
a file:line inventory of every prefix-breaking behavior, a thin CacheHints
abstraction, 7 implementation sub-slices). Three headline findings: model/
backend resolution is random per call (no session affinity — caches diluted
by construction; a sticky-by-session policy is the precondition), all cache
usage reporting is currently dropped by every adapter (measurement must land
first, riding S1's usage parcel), and our own pipeline breaks prefixes
({{date}}/{{now}} macros in the system prompt, per-turn history trimming) —
fixing determinism is cheap and unlocks the automatic caches with no wire
changes.
Hosted providers now expose the economics that our retired modeld daemon's
"effective context" research pursued: keep a stable prompt prefix resident
so repeated turns don't re-pay for it. Concretely: Anthropic prompt caching
(cache_control breakpoints), OpenAI prompt caching (automatic, plus
prompt_cache_key), Gemini context caching (explicit cachedContents objects
with TTL and storage billing), Bedrock cache points, and for local
vLLM/Ollama the client-side discipline that preserves server-side prefix
reuse. The design doc must map where OUR request pipeline breaks prefix
stability today (system-prompt assembly, tool-schema ordering, history
trimming, per-run template rewriting), propose per-provider strategies, and
define how savings are measured rather than assumed. Deliverable:
`docs/development/blueprints/provider-kv-cache.md` for maintainer review,
then implementation sub-slices.

### S2 — Capability truth — DONE 2026-07-27 (vertex embeddings IMPLEMENTED;
### bedrock CanEmbed lie also caught+fixed; pin-override now teach-refuses)
Audit findings where the model catalog lies to the request router:
- vertex advertises embedding support but its embed connection
  unconditionally errors — implement embeddings there or stop advertising;
- models an admin declares by hand (rather than auto-discovers) lose their
  vision/reasoning capability flags, because the declared-model code path
  rebuilds capabilities from a struct that never had those fields — so a
  pinned gpt-4o gets vision requests refused. Merge observed capabilities
  in additively (the ollama path already does this correctly).
Also: verify the Anthropic model catalog actually returns the capability
fields we trust it for, and show vision capability in `contenox model list`
and `contenox doctor` so a failed vision request teaches instead of
confusing.
- new finding (vision sanity-check, 2026-07-27): pinning a NON-vision model
  with `--model` while attaching an image silently overrides the pin to a
  vision model — should teach-refuse or announce the substitution, never
  swap silently.
*Gate:* a declared gpt-4o/claude routes vision correctly; embedding on
vertex either works or is refused at the catalog, never at the connection.

### S3 — Provider correctness round 2 — DONE 2026-07-27 (typed sentinels,
### Responses reasoning fixed, anthropic thinking round-trip, vllm auth,
### shared HTTP client + retry, bedrock inference profiles + Think)
Remaining audit items, in priority order:
- [ ] Typed error sentinels first: context-window-exceeded and rate-limited
      become distinguishable error values mapped per provider (today they
      are string blobs) — later context-recovery work depends on this.
- [ ] OpenAI "Responses" API: reasoning summaries are read from the wrong
      response field (a config echo), so GPT-5-class reasoning never
      surfaces; also set store:false so OpenAI doesn't retain responses
      server-side.
- [ ] Anthropic: when extended thinking and tool use combine across turns,
      our history omits the thinking blocks Anthropic requires to be sent
      back, so the follow-up request fails with a 400 — round-trip them via
      the provider-metadata field (we solved the same problem for Gemini's
      thought signatures already).
- [ ] vLLM: the auth-token config knob exists but is never passed to the
      client — authenticated vLLM endpoints cannot work.
- [ ] HTTP hygiene: every provider call uses Go's default HTTP client with
      no timeout; only OpenAI retries rate limits. Add timeouts everywhere
      and a minimal retry-with-backoff honoring Retry-After.
- [ ] Bedrock: the model catalog lists base model IDs where AWS now
      requires inference-profile IDs (so invocations fail); reasoning
      support is silently ignored — support it or refuse loudly.
*Gate:* the audit's parity matrix re-checked with no silent lies left.

### S4 — Chain & envelope vet — DONE 2026-07-27 (incl. live catch: a stale
### home-dir agent-planner.json failed vet correctly; classifier hardened to
### require array-typed keys after tokenizer vocab.json false positives)
Today a broken chain fails mid-run with internal-sounding type errors, and
nothing validates envelope (approval-policy) files at all. Build load-time
validation with teaching errors:
- a declared registry of which input/output types each task handler accepts
  and produces (currently implicit in the executor's code);
- a dataflow walk over every chain transition: where a type can never
  match, error at load with a message naming both ends ("task X produces
  json; task Y accepts string, chat_history"); where it depends on runtime
  values, keep the existing runtime check as backstop;
- reference checks (undefined variables, missing transition targets);
- the same treatment for envelope files (schema, rule shapes, timeouts);
- one `contenox vet` command covering both; validation wired into chain
  storage on write AND read, and into agent discovery, with a failing chain
  disabled-with-reason rather than failing every request.
*Gate:* the mid-run type-error class is unreachable in tests; broken files
teach at load.

### S5 — Engine event contract — DONE 2026-07-27 (step_stream_end journaled,
### hierarchical addresses, docs/development/engine-events.md is normative
### with drift-failing contract tests)
Prepares both the durable-envelope slice (S6) and the beam TUI. Three
hardening steps to the engine's event stream: an explicit "stream ended"
event carrying chunk count and usage (today the end of streaming is only a
closed channel — a replayed session can't even tell streaming happened);
hierarchical addresses on events and captured state (which chain / which
task / which tool call — needed so checkpoints and nested chains can name
positions); and a written per-event-kind field matrix that becomes THE
documented contract the beam TUI's engine-bridge consumes (today that
contract exists only implicitly inside the ACP translator code).
*Gate:* replay carries stream brackets; the documented matrix is asserted
against the translator in a test.

### S6 — Durable envelopes — DONE 2026-07-27 — **the identity slice**
Gate met (two-instance form): suspend in process A, tear down, respond in
process B → chain completes exactly once; deny variant proven; double-
respond/resume inert. Versioned checkpoint + migration engine + reflection
guard on Message fields; claim CAS picks one resumer; 30s park window
preserves interactive latency; chain_suspended event in the contract.
FOLLOW-UPS: (a) production resume-hook registration lands with the post-S6
`approvals respond` verb; (b) mission_ask_attention does NOT ride the HITL
wrapper (missiontools are HITL-exempt) — detaching attention waits needs a
park window at the attention seam + text-answer verdict injection; (c) wart:
fleet-unit topology double-files an approval into the inbox (child + parent
rows) — answering the child works, the twin expires; clean up.
Today a pending approval parks a goroutine (with the full chat history in
memory) for up to an hour, and a process restart loses the in-flight chain
entirely — the approval row survives but the work is gone. Rebuild
approvals on suspend/resume:
- when approval is needed, the engine checkpoints the run (variables,
  history, pending tool calls — all already JSON-serializable) keyed by the
  approval ID, and releases the goroutine;
- on approval, the run is rebuilt from the checkpoint and re-enters at the
  tool-execution step — reusing the engine's existing "history ends with an
  unanswered tool call" repair machinery, built for exactly this shape;
- hybrid policy: quick approvals (≤ ~30s) keep today's low-latency parked
  path; only slow ones pay the checkpoint cost;
- checkpoint format is versioned JSON with a migration hook and round-trip
  tests for every field from day one. (Cautionary precedent: the eino
  project corrupted resumed runs when its serializer silently dropped a
  pointer-typed field, and added versioning only after breaking
  compatibility — we start with both.);
- missions waiting on human attention detach instead of pinning their
  executor.
*Gate:* kill -9 during a pending approval → restart → answer the approval →
the chain completes.

### S7 — Envelope enforcement at fleet width — DONE 2026-07-27 (incl. all
### four wiring items: config key, fleet construction, doctor line, workdir
### context for relative artifact verification)
Two guards adapted from the pando mining report (`pando-mining.md`), plus
counters:
- an admission cap in fleetservice: nothing today stops a loop from
  spawning unbounded agent processes — count running units and refuse
  dispatch past a configurable ceiling with a message naming the cap and
  the remedy. (Lesson from pando: they displayed such a cap in two UIs for
  months while enforcing it nowhere — never expose a knob before it's
  enforced.);
- a conclusion verification gate: when a mission reports success and names
  produced files, stat those paths — a positively missing file downgrades
  the report from success to partial with a warning naming what's missing
  (URLs and prose claims are left alone; nothing is ever discarded). This
  catches the "done, see file X" hallucination an inbox reader can't;
- simple process-lifetime counters (dispatches, cap refusals, verification
  downgrades) exposed for doctor and the future mission panel.
*Gate:* a mission loop cannot exceed the cap; a hallucinated success
arrives downgraded.

### S8 — Context prevention: tool-output filters — DONE 2026-07-27
### (measured: 95KB go-test transcript → 430 bytes inline, failures intact)
The missing preventive leg of the context-budget story (we plan display and
recovery; nothing today reduces what a verbose command costs before it
enters history — one `go test` blast can wedge a session). Build the filter
engine specified in `pando-mining.md`: structured parsers for known formats
(go test -json, linters, tsc) that keep every failure and drop passing
noise, declarative pattern filters as fallback, a fixed transform pipeline,
stderr and exit codes structurally untouchable, filtered text inline while
the RAW output is always preserved in the existing spool (with a notice
naming the filter and the spool path), project-local filter overrides, an
inline-test validator command, and savings measured via the tracker rather
than asserted.
*Gate:* a verbose test-run transcript measurably compressed with zero
failure lines lost; the kill switch works.

### S9 — CUT (2026-07-27). Was "chain format v2" — a format redesign the
maintainer never asked for (extrapolated from "the DSL is not a contract";
that was permission, not a mandate). The current format plus S4's linter
covers the actual need. Chain composition is ruled out permanently
(maintainer: "we don't need chain composition ever — if we will need it,
transpile multiple chains together") — so no `call_chain` handler, no
declared-signature machinery; composition, if it ever matters, is a build
step that flattens chains into one chain, keeping the executor unchanged.

### S6b — Mission surfaces (maintainer ruling 2026-07-27: missions STAY,
### fix and surface them) — M
Two halves:
- **TUI parity (CORE DESIGN, named invariant):** beam surfaces ALL ACP
  slash commands — guaranteed by construction because beam's engine-bridge
  drives the real acpsvc Transport in-process, so `/mission` and every other
  command work identically in editors and the TUI. Acceptance criterion for
  beam: the command palette renders the same advertised command set an ACP
  editor receives, with zero beam-side command reimplementation.
- **CLI mission verbs reborn (in-process, no server):**
  - now: `contenox mission list/show/reports` as direct SQLite reads
    (mission records are durable), and `contenox mission fire --wait` as a
    blocking in-process dispatch;
  - after S6: `contenox approvals list/respond` and `contenox mission stop`
    — these were REST-only because a pending approval used to be a parked
    goroutine in one process's memory; once S6 checkpoints approvals to
    durable rows, any process can write the verdict and resume the run.
    S6 is the enabler; these verbs are its first payoff.

### S10 — Beam TUI service extensions — parallel after S5
The blueprint (`beam-tui.md`) lists 16 pieces of service-side work the TUI
needs before its own code starts; highest-leverage first: the libacp
library reports a max-tokens stop as a generic end-of-turn (so a UI cannot
warn that output was truncated); the in-process fleet construction lives
inside a CLI command file and must move into a service so the TUI can reuse
it; history trimming needs a service-side operation. Then TUI
implementation in the blueprint's build order — gated on the maintainer
ruling on the blueprint's 61 open design decisions (§3 below).

Interleaving: S7/S8 are independent of S1–S3. S6 needs S5.

## 2. Remaining vision (image-input) items
- [x] **Vision sanity-check PASSED (2026-07-27):** a real image went
      end-to-end — `--attach red-circle.png` → capability routing picked the
      vision model (moondream via ollama) over the text default → "I see a
      red circle." One finding filed under S2 (silent model-pin override).
      Still open below: the ACP-editor-side image run.
- [ ] **Vision sanity-check, ACP half (original item):** actually run an image
      through the whole feature against a live vision-capable model — e.g.
      `contenox --attach shot.png "what is this?"` against local Ollama with
      a vision model (llava/qwen-vl class) AND one image sent from an ACP
      editor session — and confirm the model demonstrably saw the image
      (describes its content), the session persists/replays the attachment,
      and a non-vision default model produces the teaching refusal instead
      of a confusing failure. The plumbing has unit tests but no real image
      has ever crossed the full path.
- [ ] End-to-end test: ACP image block → chain → provider wire (mock), plus
      a cost-gated real-model test (ollama container with a vision model) —
      natural rider on S1's fixture harness
- [ ] beam composer image paste (rides the TUI build; spec in blueprint)

## 2b. Copy & positioning follow-ups
- [ ] **Foreground ENVELOPES in landing (EN+DE) + README** — the envelope is
      the named concept competitors don't have; missions are what envelopes
      make safe. Today's copy leads with missions and mentions the envelope
      as supporting cast.
- [ ] Replace the landing comparison table with a three-frame terminal
      recording (mission fired → detach → envelope interrupts for approval)
      once beam exists to record it.

## 3. Open decisions (maintainer)
- [ ] **The beam blueprint's 61 open design decisions (D1–D61)** — gates TUI
      implementation start
- [x] **modelregistry: WIPED** (ruled + executed 2026-07-27): packages,
      `model registry-*` commands, model_fit and hostcapacity all deleted;
      the hand-maintained Gemini/OpenAI vision allowlists were rehomed into
      modelrepo (visiongoogle.go / visionopenai.go) — they are capability
      truth, not registry.
- [ ] **Mission re-entry** (blueprint decision D28): should a finished
      mission be able to wake its supervising agent to continue work? The
      pando report (§F1-G2/G3) contains a disciplined design: budgeted
      re-entries, batched wake-ups, an explicit await-all/any tool. If
      ruled yes → new slice after S6/S7.
- [ ] Active-blueprints cull (maintainer hinted "99% can go"; the mining
      reports and decision records should stay)
- [ ] Orphan candidates to chop or keep: internal/tooleval (+fixtures; the
      Taskfile's tool-eval target uses it), scripts/demos/,
      scripts/contenox-agentic-bench.ps1. Definite keeps:
      tools/acp-validator and libacp/cmd/acp-stub-agent (ACP conformance
      tests), tools/version (release tooling)
- [ ] Merge the statetype package into runtimestate (they were split for
      the removed HTTP API; the split now serves nothing)
- [ ] Dependabot: 67 findings on the default branch (8 critical) — triage;
      likely the website's npm tree (`cd website && npm audit`)
- [ ] AWS: decommission the old modeld artifact bucket
      (contenox-modeld-artifacts-573643652148)?
- [ ] After the next release: verify install.sh end-to-end against
      contenox/beam release assets · demo recording scripts
      (scripts/demos/*.tape) still reference the pre-rename checkout path

## 4. Stale-hunt & hygiene (follow-up sessions)
Greps to run; every hit must be justified or removed:
- `grep -rniE 'modeld|openvino|llamacpp' --include='*.go' .` — known hits:
  the modelregistry question above; two comment-level mentions in
  internal/surfaces/acpsvc (config_options.go:434, prompt.go)
- `grep -rniE 'mistral|openrouter' .` — after S1's removal lands, only git
  history should know them
- `grep -rniE 'contenox/runtime' .` — the pre-rename module path; the demo
  tapes are the known remainder
- `grep -rniE 'beam' docs/ website/ README.md` — every hit must mean the
  TUI or the Retired-R&D page, never the dead web UI
- `grep -rn 'make ' docs/ CONTRIBUTING.md README.md .github/` — build docs
  should say task, not make
- go.mod: the charmbracelet TUI libraries must only appear when beam's code
  starts; grpc must stay indirect-only
- Flaky-under-load tests (all pass in isolation; harden or serialize):
  runtimetypes TestUnit_Backend_DeletesSuccessfully (a Postgres
  testcontainer times out under parallel load — and why does the
  SQLite-era store still test against Postgres at all?), acpsvc
  TestE2E_Wire_ExternalAgent_CommandMenuAfterNewSessionResult and
  TestLoopback_ExternalAgent_LazyRespawnPushesConfigOptions (subprocess
  EOF under load)
- examples/ and .contenox/ chains re-verified against surviving providers
- SUPPORT.md and .github issue templates checked for references to removed
  surfaces
- Small audit leftovers: a stale modeld mention in llmrepo's doc comment;
  an unused Vertex mode in the Anthropic messages codec; vLLM sends tool
  names unsanitized (dots can break some chat templates)
