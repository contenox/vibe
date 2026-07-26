# Blueprint: Provider KV-Cache Utilization (S1b)

> Status: design for review (TODO.md §S1b). Heritage: the retired modeld
> effective-context R&D (`docs/development/blueprints/retired/modeld/…`, recovered
> from git history at `7ffadb05^`). Scope: the seven surviving providers —
> ollama, vllm, openai, anthropic, gemini, bedrock, vertex. mistral/openrouter
> are dead and out of scope.

## 0. The bet, restated

modeld's effective-context program proved one economic fact on local hardware:
**the stable prefix of an agent loop is ~everything, and recomputing it every
turn is the dominant cost**. The S-series prefix-cache benchmark measured
~99.5% warm reuse on the agent-loop shape (stable system/tools/repo prefix,
turns appended). modeld died; the economics did not. Every surviving provider
now exposes the same lever — server-side prefix/KV caching — priced at roughly
**0.1×–0.25× per cached input token** (hosted) or **free TTFT/throughput wins**
(local). We already pay full price for the same bytes on every turn of every
mission. This slice makes the runtime *cache-shaped*: deterministic prefixes on
our side, provider-native cache controls on theirs, measured hit rates in the
tracker.

## 1. Provider facts (researched 2026-07; cite-checked)

<!-- FACTS:PROVIDERS -->

### Anthropic (first-party API)

- `cache_control: {"type": "ephemeral"}` breakpoints on content blocks (system
  text blocks, tool definitions, message blocks). Max **4 breakpoints** per
  request. Default TTL **5 minutes** (sliding on read); `"ttl": "1h"` variant.
- Render order is `tools → system → messages`; a breakpoint on the last system
  block caches tools+system together. Cache key = exact bytes of the rendered
  prefix up to the breakpoint; caches are **model-scoped**.
- Minimum cacheable prefix is model-dependent: 1024 tokens on most current
  models (512 on the newest Opus tier; 2048/4096 on some older models).
  Shorter prefixes silently don't cache (`cache_creation_input_tokens: 0`).
- Pricing: cache **write 1.25×** base input (5m TTL) / **2× (1h TTL)**; cache
  **read ~0.1×**. Break-even at 2 requests (5m) / 3 requests (1h).
- Usage report: `usage.cache_creation_input_tokens`,
  `usage.cache_read_input_tokens`; `usage.input_tokens` is the *uncached
  remainder only* — total prompt = sum of the three.
- Invalidation hierarchy: tool-definition or model change kills everything;
  system content kills system+messages; message content kills messages only.
  `tool_choice`/thinking-toggle do NOT kill the tools+system cache.

### OpenAI

(Docs: developers.openai.com/api/docs/guides/prompt-caching; Cookbook
"Prompt Caching 201", 2026-02-18; pricing page — all cite-checked 2026-07.)

- **Pre-GPT-5.6 models** (gpt-4o and newer): caching is fully automatic for
  prompts **≥1024 tokens**, hits in **128-token increments**, cache writes
  free. TTL: 5–10 min of inactivity, up to 1h off-peak (in-memory);
  `prompt_cache_retention: "24h"` extends to 24h at no extra price on
  gpt-4.1/gpt-5.x families.
- **GPT-5.6+ models**: `prompt_cache_key` is **required** for reliable cache
  matching; cache **writes billed at 1.25×** input (new `cache_write_tokens`
  usage field); TTL `prompt_cache_options.ttl: "30m"` (only value, default);
  optional Anthropic-style explicit breakpoints
  (`prompt_cache_options.mode: "explicit"` + per-block
  `prompt_cache_breakpoint`, ≤4 writes/request, prefix before a breakpoint
  still ≥1024 tokens). Older models 400 on these params.
- `prompt_cache_key`: routing is by hash of the first ~256 tokens combined
  with the key; keep traffic **≈15 req/min per key** or requests spill to
  other shards (one-time miss each). Per-user or per-conversation keys are
  the documented best practice (measured 60%→87% hit-rate case). The `user`
  field is deprecated for this purpose — "use `prompt_cache_key` instead".
- Discounts vary by family: gpt-4o **50%**, gpt-4.1 **75%**, gpt-5.x **90%**
  (e.g. gpt-5.6-luna $1.00 input / $0.10 cached / $1.25 cache-write per 1M).
- Usage report: Chat Completions `usage.prompt_tokens_details.cached_tokens`;
  Responses API `usage.input_tokens_details.cached_tokens` (+
  `cache_write_tokens` on 5.6+). Responses API gets 40–80% better cache
  utilization for reasoning models (reasoning items persist server-side).
- Exact-prefix semantics: tools array, system message, images
  (incl. `detail` param), and structured-output schemas are all part of the
  prefix; any earlier-byte change forfeits the hit. Cached tokens still
  count toward TPM rate limits.

### Gemini (Developer API) and Vertex AI

(Docs: ai.google.dev/gemini-api/docs/caching + /generate-content/caching +
/api/caching + pricing; cloud.google.com Vertex context-cache pages —
cite-checked 2026-07.)

- **Implicit caching**: enabled by default on all Gemini 2.5+ models, savings
  passed on automatically, no storage cost, best-effort (no hit guarantee).
  Minimums: 2,048 tokens (2.5 Pro/Flash), 4,096 (3.x-series). Current
  discount is **~90%** on cached tokens (the old 75% figure is 2.0-era).
  Hits reported via `usageMetadata.cachedContentTokenCount`.
- **Explicit caching**: `cachedContents` objects
  (`POST …/v1beta/cachedContents`) built from `model` + `contents[]` +
  `systemInstruction` + `tools`/`toolConfig` (all immutable after create);
  referenced via `cachedContent` on `generateContent`; **only expiration is
  updatable** (`caches.update` with new `ttl`/`expireTime`). Default TTL
  **1h**. Same per-model minimums as implicit. Billing = cached tokens at
  ~10% of input price **plus storage per token-hour** (2.5 Pro
  $4.50/1M tok/hr; Flash-class $1.00/1M tok/hr). Explicit gives a cost
  *guarantee* vs implicit's best-effort.
- **Vertex AI** mirrors both: implicit on by default (disable-able for data
  governance), 90% discount on 2.5-class models; explicit cachedContents are
  project/region-scoped resources, default expiry 60 min, storage billed
  hourly, **not supported with Provisioned Throughput**. Minimum cache size
  figures conflict in the docs (2,048 vs 4,096) — pin at implementation
  time.

### AWS Bedrock

(Docs: docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html +
pricing page — cite-checked 2026-07.)

- `cachePoint` blocks (`{"cachePoint": {"type": "default"}}`) in the Converse
  API — placeable in `system`, `messages`, and `tools`; processing order is
  `tools → system → messages` (changing an earlier section invalidates later
  sections); up to **4 checkpoints**; minimum size evaluated on cumulative
  tokens across sections. Below-minimum checkpoints don't error — the prefix
  just isn't cached.
- TTL: **5 min sliding** (resets on hit, refresh free); opt-in **1h TTL**
  (`"ttl": "1h"`) on Claude Opus/Sonnet/Haiku 4.5-class models; 1h entries
  must precede 5m entries.
- Supported models: Anthropic Claude (3.5 Sonnet v2 / 3.7 onward; minimums
  1,024 tokens on e.g. Sonnet 4.6, 4,096 on the 4.5/Opus 4.6 class), Amazon
  Nova (also caches text automatically without cachePoints), and OpenAI
  GPT-5.x via bedrock-mantle. On-demand inference only (no batch). Claude
  gets "simplified cache management": one checkpoint at the end of static
  content; hit lookup checks prior block boundaries (~20-block lookback).
- Pricing (Claude): cache read **−90%**, 5m cache write **+25%**, 1h cache
  write **2×** input. Nova cache pricing unverified on the public page.
- Usage report: `usage.cacheReadInputTokens` / `usage.cacheWriteInputTokens`
  (+ `cacheDetails`); `inputTokens` counts only the uncached remainder —
  total = sum of the three. Works via Converse/ConverseStream and
  InvokeModel (Claude-native `cache_control`).

### vLLM (self-hosted)

(Docs: docs.vllm.ai design/prefix_caching + features/automatic_prefix_caching
+ design/metrics; source `vllm/config/cache.py` — cite-checked 2026-07.)

- Automatic Prefix Caching (APC): **enabled by default in vLLM v1**
  (`enable_prefix_caching: bool = True`; disable via
  `--no-enable-prefix-caching`). Block-level, hash-chained: each KV block's
  key = hash(parent hash, block token IDs, extra keys) — so any earlier-token
  change invalidates every downstream block; partial blocks aren't reused.
  Eviction is LRU. Server-side and shared across all clients automatically
  (isolation only via the opt-in `cache_salt` request field); extra keys
  include LoRA IDs and multimodal hashes, so LoRA variants never share.
- Reuse requires exact token-prefix match *after chat-template rendering* —
  the chat template serializes system + tools into the prompt, so our
  client-side contract is identical to hosted providers: stable system bytes
  and stable tool ordering.
- Measurement: Prometheus counters `vllm:prefix_cache_queries` /
  `vllm:prefix_cache_hits` (the V0 `gpu_prefix_cache_hit_rate` gauge is
  legacy; compute rate from the counters). **Do not rely on
  `usage.prompt_tokens_details.cached_tokens`**: `--enable-prompt-tokens-details`
  is broken on the V1 engine (always null — vllm#44961, still open
  mid-2026). Benefit is TTFT/throughput only; no billing dimension.

### Ollama (self-hosted)

(Docs: docs.ollama.com/faq; source `llm/llama_server.go`, `server/sched.go`;
llama.cpp tools/server README — cite-checked 2026-07.)

- Current ollama wraps upstream llama.cpp `llama-server` and sends
  `"cache_prompt": true` on every request: **per-slot, last-request
  longest-common-prefix reuse** — a slot keeps its previous request's KV and
  re-prefills only the divergent suffix. Slot selection by prompt similarity
  (`--slot-prompt-similarity`, default 0.10).
- `OLLAMA_NUM_PARALLEL` defaults to **1 slot** (total ctx = num_ctx ×
  parallel). With one slot, **interleaving two sessions on one model
  thrashes**: each alternation diverges near token 0 and re-prefills the
  whole history. Session→backend/model affinity is the entire lever.
- Residency: `keep_alive` default 5m (per-request or `OLLAMA_KEEP_ALIVE`;
  `-1` = forever); `OLLAMA_MAX_LOADED_MODELS` default 3×GPUs — loading
  another model can evict an idle runner and its whole KV. Runner **restart
  (full KV loss)** on changes to `num_ctx`, `num_batch`, `num_gpu`,
  `use_mmap`, context-shift, or adapter paths; sampling options
  (temperature) are per-request and safe. `OLLAMA_KV_CACHE_TYPE`
  (f16/q8_0/q4_0) trades KV memory for capacity.
- Measurement: llama-server reports `timings.cache_n` (tokens reused);
  ollama's `prompt_eval_count` = cache_n + prompt_n, i.e. **the reported
  prompt count includes cached tokens** — cache_n is the per-request warm
  signal if surfaced; otherwise TTFT deltas are the observable.

<!-- /FACTS:PROVIDERS -->

## 2. Heritage: what modeld taught us, reborn

Recovered concepts from the retired effective-context blueprints
(`north-star.md`, `architecture.md`, `benchmark-integrity-blueprint.md`) and
the surviving `internal/kernel/contextasm` package:

| modeld concept | Then (local KV) | Now (provider caches) |
|---|---|---|
| **Stable-prefix doctrine** — segments ordered stable→volatile (`SegmentKind`: System, Tools, RepoRules, RepoMap, Pinned, Diff, Terminal, UserTurn; `contextasm/segments.go:12-23`) | Render stable-first so `EnsurePrefix` reuses resident KV, re-prefill only the tail | Render stable-first so the provider's prefix hash matches; place breakpoints at the stable/volatile boundary |
| **Cache key = manifest, not bytes** — `ContextManifest`: runtime identity + `StableTokenHash` ("byte equality alone is never enough") | Gated warm reuse across sessions | Provider caches are keyed on (model, exact prefix bytes[, key]) — our side must guarantee *byte determinism* and must know that a model/provider switch is a full cold start |
| **`CacheClass`** (`task_pinned` / `repo_map` / `volatile`, `MoreEvictableThan`) | Eviction priority when context exceeds VRAM | Breakpoint placement priority: `task_pinned` behind the deepest breakpoint; `volatile` after the last one; trimming (Shift) must eat `volatile` before it perturbs anything cached |
| **Invalidation hints** (`on_edit`, `on_turn`) | Drop exactly the invalidated KV | Know *which* provider tier a change invalidates (tools > system > messages) and never pay a full invalidation for a suffix-only change |
| **Warm-vs-cold accounting** (benchmark-integrity: warm/cold state is a labeled dimension; never compare unmatched rows) | ~99.5% warm reuse measured on the agent-loop shape | `cached_tokens` extraction per provider → tracker; hit-rate is a first-class product metric with warm/cold labeled, not inferred |
| **Single-slot / stickiness** (one model, one user, resident state authoritative) | `modeld/slot` single active model | Session→backend affinity for ollama/vllm: the cache lives in one process; round-robining a session across replicas is a guaranteed miss |

The division of labor also survives: *offload the tensor mechanics to the
engine (now: the provider); own the semantics in the runtime.* The runtime is
the only party that knows which bytes are the durable task core — that
knowledge is now expressed as breakpoint placement and serialization
discipline instead of a manifest on the wire.

## 3. Our request path today: every prefix-breaking behavior

Engine-side inventory (provider-side in §3b). "Tier killed" uses the
Anthropic hierarchy (tools > system > messages) as the general model — the
same ordering governs OpenAI/vLLM byte-prefix matching.

| # | Behavior | Where | Tier killed | Frequency |
|---|---|---|---|---|
| E1 | `{{date}}` / `{{now}}` macros expand wall-clock time into `SystemInstruction` / `PromptTemplate` | `internal/kernel/taskengine/macroenv.go:269-280` | system (+everything after) | every request (with `{{now}}`) or daily (`{{date}}`) — a classic silent invalidator |
| E2 | System-instruction auto-appends: tools summary (`Available tools …` from live registry), TOOL PREFERENCE nudge, `Host: os= arch=` line | `macroenv.go:114-134` | system | whenever the tool registry answer changes; `renderToolsAndToolsJSON` marshals a map (Go sorts keys — OK) but the inner name arrays follow registry order — registry order must be pinned |
| E3 | `HistoryTrim` keeps the last N messages — a sliding window that changes the first history message every turn once len>N | `internal/services/agentservice/agent.go:313-315` | messages (entire history tier) | every turn once active |
| E4 | `shiftMessagesToFit` drops oldest non-system units on overflow | `internal/kernel/taskengine/taskexec.go:173-240`, decision at `taskexec.go:1147-1170`, route variant `:547` | messages | every overflowing turn; each shift is a new prefix |
| E5 | AGENTS.md injected as first history message *only when history is empty* | `agent.go:317-322` | — (stable once present) | persisted into history — good; but any re-derivation that changes bytes (file edit) invalidates from message 0 |
| E6 | MacroEnv per-run rewrites of `ExecuteConfig.Model` / `Provider` / `Think` via `{{var:*}}` | `macroenv.go:139-157` | **model switch = total** | whenever caller vars change model/provider; Think toggles are provider-dependent (Anthropic: thinking toggle does *not* kill tools+system; Gemini/OpenAI: parameter changes are outside the prefix) |
| E7 | Tool allowlists resolved per task → `[]Tool` order = registry/allowlist order; no canonical sort at the seam | `taskexec.go` chatArgsForLLMCall (`taskexec.go:1370-1394`) + `toolsproviderservice` | tools (= everything) | whenever registry enumeration order wobbles or the allowlist is reordered |
| E8 | `ComposeUserInput` prepends per-turn artifacts to the *current* user message | `agent.go:274-297` | — (suffix only) | none — correct placement, keep it that way |
| E9 | Client-side token pre-count drives shift; provider-reported usage unused — no `cached_tokens` parsing exists anywhere (grep confirms zero hits) | `taskexec.go:1120-1145` | — (measurement gap) | we cannot currently *see* a hit or miss |

### 3b. Provider-side inventory (adapter survey, cite-checked)

**The single biggest finding is upstream of every adapter: resolution is
uniformly random per call.** `llmrepo.Chat/Stream/PromptExecute/Embed` all
pass `llmresolver.Randomly`
(`internal/models/llmrepo/llmrepo.go:202,256,306,361`), which draws
`rand.Intn` over matching providers *and* over a provider's backends
(`internal/kernel/llmresolver/requestresolver.go:417-460`), and
`llmrepo.Request` (`llmrepo.go:20-25`) carries **no session identity at
all** — there is nothing affinity could even key on. A fresh client is
constructed and closed per request. Consequences: a multi-backend
ollama/vllm provider hits its prefix cache at rate `1/len(backends)` by
luck; and when several provider instances match a model, even hosted-cache
hits are diluted across instances (distinct API keys/regions = distinct
caches).

| # | Behavior | Where | Impact |
|---|---|---|---|
| P1 | Random provider+backend per call, no session key, client per request | `llmrepo.go:202,256`; `requestresolver.go:443-460` | kills local KV reuse; dilutes hosted caches; blocks `prompt_cache_key` |
| P2 | **No cache field exists anywhere**: zero non-test hits for `cache_control` / `cachePoint` / `cachedContent` / `prompt_cache` / `keep_alive` across `modelrepo/**`. Anthropic codec structs (`codec/messages/messages.go:58-71,82-86`) have no `cache_control` slot; bedrock never constructs the SDK's `CachePoint` members | repo-wide grep | greenfield — struct additions needed per adapter |
| P3 | **Usage is dropped everywhere.** Anthropic response struct parses no `usage`; openai declares no usage field; gemini/vertex omit `usageMetadata`; bedrock ignores `out.Usage` (which the SDK types already carry incl. `CacheRead/WriteInputTokens`, `converse.go:173-208`) and skips the stream `Metadata` event; ollama drops `PromptEvalCount`; vllm parses usage but drops it in Chat (`vllm/chat.go:41-69`). `ChatResult`/`StreamParcel` have no usage field | per-adapter | S1's terminal parcel + S3-I7 is the carrier; cache fields ride it |
| P4 | Tool `Parameters` is `map[string]any` almost everywhere (mcpconvert.go:19-22, fs_schema.go:138) → `encoding/json` alphabetizes keys: **deterministic** across requests (good), and all 7 adapters preserve tool slice order | cross-cutting | determinism holds today at the params level; the risk is upstream slice order (E7) |
| P5 | Ollama adapter never sets `keep_alive` (SDK field exists, `api/types.go:161-163`) → server default 5m always | `ollama/chat.go:91-98` | residency shorter than agent think-time; not configurable today |
| P6 | vLLM serializes the neutral `Message` struct raw — history `thinking` and tool-call `provider_meta` (e.g. a Gemini `thought_signature`) leak onto the wire, presence flipping with history provenance | `vllm/client.go:196-216` | prefix perturbation on provider migration + protocol hygiene bug |
| P7 | Tool-call IDs are minted with `uuid.NewString()` at decode in gemini/vertex/ollama; openai/anthropic/bedrock/vllm serialize `ToolCall.ID` back out | `gemini/chat.go:93`, `vertex/chat.go:76`, `ollama/chat.go:174` | stable *within* a session (minted once, stored), but any regeneration/replay produces new bytes in an otherwise-stable prefix |
| P8 | Gemini Stream drops the system prompt entirely (`buildGeminiRequest(..., nil, ...)` + system skipped in message convert) | `gemini/stream.go:23`, `gemini/client.go:282-284` | known C2, fixed in S1 — prerequisite for any Gemini caching claim |
| P9 | Think/effort injection is per-model-family request *fields* (anthropic `thinking` block, openai `reasoning_effort`, gemini `thinkingConfig`, ollama `think`), never spliced into system text | e.g. `anthropic/client.go:103-152` | good: on Anthropic a thinking toggle does not invalidate the tools+system cache tier |

## 4. Design

### 4.1 The stability contract (engine-side; prerequisite for everything)

A single documented invariant, enforced at the seams, testable without any
provider:

> **Deterministic serialization contract.** For a fixed (session, chain,
> registry state), the rendered request prefix — system instruction bytes,
> tool list order and JSON encoding, history prefix — is byte-identical
> across turns, and each turn's request extends the previous turn's request
> by appendage only, until an explicit trim event.

Concretely:

1. **Kill the wall-clock in the prefix.** `{{date}}`/`{{now}}` stay available
   but the docs and `contenox vet` (S4) flag them in `SystemInstruction` /
   prompt prefixes; the agent chain templates we ship move date injection to
   the user turn (suffix). Cheap, huge win (E1).
2. **Canonical tool serialization.** One function
   (`modelrepo.CanonicalizeTools`) sorts tools by name, normalizes the
   parameters JSON deterministically (Go maps already marshal key-sorted;
   `json.RawMessage` passthroughs must be re-canonicalized), and every
   adapter serializes from that slice in order. Registry enumeration order
   stops mattering (E2, E7).
3. **Trim is an event, not a drift.** `HistoryTrim` and `shiftMessagesToFit`
   remain, but become *chunked*: drop in blocks (e.g. 25% of budget) instead
   of one unit per turn, so a trim produces one cold write followed by many
   warm turns instead of a cold write every turn (E3, E4). The drop boundary
   respects `contextasm.CacheClass`: volatile units go first (heritage rule).
4. **Model/provider identity is part of the cache key.** When MacroEnv vars
   flip model/provider mid-session (E6), that is a legitimate cold start —
   but the tracker must label it as such (warm/cold accounting) so it never
   masquerades as a caching bug.
5. **Session affinity in resolution (fixes P1).** `llmrepo.Request` gains an
   optional `SessionKey string`; a new `llmresolver.Sticky(key)` policy
   hashes the key over the healthy candidate set (rendezvous/HRW hashing so
   a backend loss only remaps that backend's sessions) and replaces
   `Randomly` on the chat/stream paths when a key is present. No key ⇒
   `Randomly`, exactly as today. This one change is a precondition for
   ollama/vllm reuse *and* for hosted-cache consistency across
   multi-instance providers.
6. **Stop leaking provenance bytes (fixes P6).** vLLM (and any
   OpenAI-shaped adapter) serializes an explicit wire struct, never the
   neutral `Message` — `thinking` and `provider_meta` must not reach the
   wire.

### 4.2 The abstraction: thin, omission-tolerant CacheHints

No manifest resurrection, no per-provider leakage into the engine. One
optional field on `ChatConfig`
(`internal/models/modelrepo/modelprovidertypes.go:96`):

```go
// CacheHints tells a provider where the stable/volatile boundary of this
// request lies. Providers map what they can and ignore the rest — omission
// changes nothing.
type CacheHints struct {
    // SessionKey is a stable opaque key for cache-shard affinity
    // (OpenAI prompt_cache_key; ollama/vllm backend stickiness).
    SessionKey string
    // StableSystem marks the system instruction as byte-stable across the
    // session (safe to place a breakpoint after it).
    StableSystem bool
    // StableTools marks the tool list as byte-stable across the session.
    StableTools bool
    // StableHistoryLen is the number of leading history messages the engine
    // asserts are unchanged since the previous request of this session
    // (0 = no assertion). Providers with explicit breakpoints may mark the
    // last of these.
    StableHistoryLen int
    // TTL is an advisory lifetime ("5m", "1h") for providers with explicit
    // TTLs (anthropic ttl variant, gemini cachedContents). Empty = default.
    TTL string
}
```

Populated in one place — `chatArgsForLLMCall` / the agent chat path — from
facts the engine already has (session ID, whether this turn appended-only,
whether trim fired). `ChatArgument` gets `WithCacheHints`. **Semantics rule:
hints may never change what the model sees** — only cache metadata
(breakpoints, keys, cachedContent references) differs. Any provider that
would have to *reorder or rewrite content* to honor a hint must ignore the
hint instead.

### 4.3 Per-provider strategy

| Provider | Mechanism | What the adapter does with CacheHints | Cost model note |
|---|---|---|---|
| **anthropic** | `cache_control` breakpoints (max 4) | Add `CacheControl` fields to `wireTool`/`wireBlock`/system in `codec/messages` (P2). BP1 after last tool def (StableTools); BP2 after last system block (StableSystem — note our codec joins all system messages into one string field, so this is one breakpoint on the system value); BP3 on the last content block of the previous turn's final message (StableHistoryLen), giving incremental per-turn reuse; keep the 4th in reserve for a `ttl:1h` long-session variant | write 1.25×/2×, read 0.1×; only place breakpoints when hints say stable — a breakpoint on churning content pays the write premium for nothing |
| **bedrock** | `cachePoint` blocks in Converse (max 4, ~5m TTL) | same placement as anthropic (system / tools / stable-history); gate on supported model IDs (Claude, Nova) — unsupported model ⇒ omit silently | read ~0.1×, Claude write ~1.25× |
| **openai** | automatic ≥1024-token prefix + `prompt_cache_key` (both chat-completions and responses endpoints) | set `prompt_cache_key = hash(tenant, SessionKey)` on both request structs; nothing else for pre-5.6 models — the win comes entirely from §4.1 determinism. GPT-5.6+ semantics (required key, billed writes, 30m TTL, explicit breakpoints) are a follow-up once we route those models; the key we already send is the prerequisite either way | reads 0.5×(4o)/0.25×(4.1)/0.1×(5.x); pre-5.6 writes free — always safe; 5.6+ writes 1.25× — same placement caution as anthropic |
| **vertex / gemini** | implicit caching (2.5+) + explicit `cachedContents` | Phase 1: rely on implicit caching only (needs §4.1). Phase 2 (only if measurement proves implicit insufficient): explicit `cachedContents` for sessions with large stable prefixes (> model minimum) — adapter creates cache from system+tools+stable history, stores {resource name, expiry, prefix hash} in a small session-scoped registry, refreshes TTL on use, recreates on prefix-hash change, and deletes on session end. **Owner of TTL refresh: the adapter, lazily on request** — no background reaper; an expired cache is just a cold start | implicit: ~90% discount on 2.5+ models, free; explicit: discount minus storage per token-hour — the accounting must net these before claiming savings |
| **ollama** | per-slot last-request LCP reuse (`cache_prompt:true` is already always sent) | (a) `Sticky` resolution (§4.1.5); (b) start setting `keep_alive` (P5): configurable per backend, default ≥ the 5m server default, so a thinking user doesn't cold-start; (c) never vary runner-level options (`num_ctx`, `num_batch`, …) between turns — those restart the runner and lose all KV; (d) surface `timings.cache_n`/`PromptEvalCount` into Usage | free; benefit = TTFT (prefill skipped) |
| **vllm** | automatic prefix caching (default on, v1) | `Sticky` resolution + §4.1 determinism + wire-struct fix (P6). Do **not** trust `prompt_tokens_details.cached_tokens` (broken/null on V1, vllm#44961) — doctor scrapes `vllm:prefix_cache_queries`/`vllm:prefix_cache_hits` counters instead; adapter-level warm signal is TTFT | free |

### 4.4 Measurement (rides S3's usage extraction, I7)

Extend the S1 terminal parcel / S3 usage struct with cache fields:

Today `ChatResult`/`StreamParcel` carry no usage at all and every adapter
drops what the wire already offers (P3) — bedrock's SDK response even has
`CacheRead/WriteInputTokens` typed and ignored. S1's terminal parcel gains a
`Usage` struct; S1b extends it:

```go
type Usage struct {
    PromptTokens     int
    CompletionTokens int
    CacheReadTokens  int // anthropic cache_read_input_tokens; openai
                         // prompt_tokens_details.cached_tokens (chat) /
                         // input_tokens_details.cached_tokens (responses);
                         // gemini/vertex usageMetadata.cachedContentTokenCount;
                         // bedrock cacheReadInputTokens; ollama: llama-server
                         // timings.cache_n if surfaced (note ollama's
                         // prompt_eval_count INCLUDES cached tokens)
    CacheWriteTokens int // anthropic cache_creation_input_tokens; bedrock
                         // cacheWriteInputTokens; openai 5.6+ cache_write_tokens;
                         // 0 elsewhere
}
```

Provider caveats: vLLM's `cached_tokens` is null on the V1 engine
(vllm#44961) — its warm signal is server Prometheus counters + TTFT, so the
doctor probe, not per-request usage, owns vLLM measurement. Note the
"uncached remainder" convention: on anthropic and bedrock the plain input
count excludes cached/written tokens (total = sum of three), while openai's
`prompt_tokens` *includes* `cached_tokens` — normalization lives in the
adapter, and `Usage.PromptTokens` is defined as **total prompt tokens** on
every provider.

- Tracker: emit on every chat/stream terminal event
  (`token_usage` gains `cache_read`/`cache_write`); warm/cold is *labeled*
  (heritage rule): first-turn and post-trim requests are expected-cold,
  mid-session appends are expected-warm — a cold result on an expected-warm
  turn is the bug signal.
- `contenox status` / `doctor`: per-provider session hit-rate
  (`cache_read / (prompt - cache_write)` over expected-warm turns); doctor
  gains a "cache health" probe: run a 3-turn canned session against a
  provider, assert turn-2/3 report nonzero cache reads (hosted) or improved
  TTFT (local).

### 4.5 Envelope / policy interaction (ComputeBounds)

`ComputeBounds.MaxTokens` (`internal/services/hitlservice/policy.go:186`,
enforced best-effort from reported usage in
`fleetservice.go:465,487-498`) is a **compute-spend ceiling**, and cached
reads are 10× cheaper compute. Decision:

- **Bounds keep counting raw tokens.** MaxTokens stays a context/work
  ceiling — deterministic, provider-independent, and immune to a provider
  silently changing discount rates. Changing its meaning per provider would
  make the same envelope permit 10× more work on anthropic than on ollama.
- **Add a separate, additive `MaxCostWeightedTokens` (opt-in, later)** if an
  operator wants spend-shaped bounds: `raw - cache_read*(1-w)` with a
  per-provider weight defaulting to 0.1 for hosted / 1.0 for local. Not in
  S1b's implementation scope; the envelope schema note lands with the design
  so the field name is reserved.
- Usage events forwarded over ACP (`usage_update`) carry the cache fields so
  a *client-side* cost display can show real spend without the bound
  semantics changing.

## 5. Slice plan

Dependencies: S1 (raw-delta streaming contract + terminal usage parcel) must
land first — cache fields ride the same parcel. S3's I7 usage extraction is
the natural carrier for §4.4; S1b.2 can land with S3 or immediately after.

| Sub-slice | Content | Effort | Depends on | Gate |
|---|---|---|---|---|
| **S1b.1 Determinism** | Canonical tool serialization at the `ChatConfig` seam; kill `{{now}}`/`{{date}}` from shipped chain prefixes + vet warning; chunked trim for HistoryTrim/Shift (CacheClass-ordered); vLLM wire-struct fix (P6: no `thinking`/`provider_meta` leakage); byte-stability unit test: render the same session twice per adapter, assert identical prefix bytes and append-only growth | **M** | S1 | golden test proves byte-identical request prefixes across 10 simulated turns incl. one chunked trim (exactly one prefix change), per adapter |
| **S1b.2 See it** | Usage struct cache fields on the S1 parcel; parse per-provider usage (anthropic, openai, gemini, vertex, bedrock, ollama; vllm via doctor/Prometheus); tracker + `status` surfacing with warm/cold labels | **S/M** | S1 (parcel), pairs with S3-I7 | repeated-session fixture against openai + anthropic shows `cache_read > 0` reported end-to-end in tracker events |
| **S1b.3 Sticky resolution + SessionKey plumbing** | `llmrepo.Request.SessionKey`; `llmresolver.Sticky` (rendezvous-hash over healthy candidates, fallback `Randomly`); agent/chat paths thread the session ID (HMAC'd) through | **S/M** | none (parallel to S1b.1) | same-session fixture resolves to the same backend across 20 turns with 3 backends registered; backend removal remaps only its own sessions |
| **S1b.4 Anthropic + Bedrock breakpoints** | `CacheHints` type + `WithCacheHints`; engine populates hints; `codec/messages` gains `cache_control` struct fields; anthropic places ≤4 breakpoints; bedrock `cachePoint` with model gating | **M** | S1b.1, S1b.2 | measured cache-hit rate **>80% of input tokens on turns ≥3** of a 5-turn repeated-session fixture vs live anthropic; bedrock fixture shows nonzero `cacheReadInputTokens` |
| **S1b.5 OpenAI key + local hygiene** | `prompt_cache_key` on both openai endpoints from SessionKey; ollama `keep_alive` config surfaced (P5); ollama cache_n → Usage if reachable | **S** | S1b.1–3 | openai fixture: `cached_tokens > 0` by turn 2; ollama: TTFT turn-3 < 30% of TTFT turn-1 on a long-prefix fixture against a local instance |
| **S1b.6 Gemini/Vertex explicit caching** | Only if S1b.2 measurement shows implicit caching underperforming (<50% of stable prefix on warm turns): `cachedContents` lifecycle in the adapter (create/reference/TTL-refresh/recreate-on-change), session-scoped registry, storage-cost netting in tracker | **M/L** | S1b.2 evidence | gemini fixture hit-rate >80% on warm turns AND netted cost (discount − storage) positive on the fixture profile |
| **S1b.7 Doctor + docs** | doctor cache-health probe (hosted: usage fields; vllm: Prometheus counters; ollama: TTFT delta); provider docs page: what we do per provider, what invalidates, how to read the numbers | **S** | S1b.2–5 | doctor flags a deliberately cache-hostile chain (`{{now}}` in system) with a teaching message |

## 6. Risks & non-goals

- **Never let cache placement change semantics.** Breakpoints, keys, and
  cachedContent references must be metadata-only. If honoring a hint would
  require reordering content a chain author wrote, the hint is dropped. A
  golden test asserts request bodies with and without CacheHints are
  identical modulo cache metadata fields.
- **Write-premium asymmetry.** Anthropic/Bedrock cache writes cost 1.25–2×.
  Placing breakpoints on volatile content is *worse* than doing nothing.
  Mitigation: breakpoints only on hint-asserted-stable content; hit-rate
  measurement makes regressions visible.
- **TTL churn.** 5-minute TTLs vs. human-paced sessions: a user thinking for
  6 minutes pays a full re-write. We do not build keepalive pingers in S1b
  (spend to save is a policy decision, not a default); 1h TTL is opt-in via
  hints where a chain declares a long session. Gemini explicit caches add
  storage-per-hour — hence the evidence gate on S1b.6.
- **Local ≠ hosted failure modes.** ollama/vllm reuse silently degrades with
  concurrency/slot pressure; that's a deployment property, not a bug we can
  fix from the client. Doctor reports it; we don't chase it.
- **Non-goal: resurrecting the manifest wire format.** No token hashes, no
  segment protocol to providers. Providers only ever see standard API fields.
- **Non-goal: cross-session/global caching.** Shared-prefix caching across
  tenants/sessions (e.g. a fleet of missions sharing one system prompt) is a
  natural follow-up but needs the multi-mission fixture from S7-era work;
  S1b optimizes within a session.
- **Non-goal: envelope semantics change.** MaxTokens meaning is unchanged
  (§4.5); cache-aware cost bounds are reserved, not implemented.

## 7. Review questions for the maintainer

1. Chunked-trim block size: fixed fraction (25% of budget) or CacheClass-aware
   (drop all volatile first, then chunk)? Heritage says the latter.
2. Should `SessionKey` be the chat session UUID directly or an HMAC of it
   (avoids leaking internal IDs to providers)? Proposal: HMAC.
3. Is S1b.6 (Gemini explicit caches) worth its lifecycle complexity at our
   session lengths, or do we accept implicit-only and revisit? The evidence
   gate is designed to let the data answer.
