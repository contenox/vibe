# Inference stack decision: vLLM, modeld, or both — 2026-07-28

The core engineering choice underneath all three deliverables of the private
repo (hosted free tier, paid GPU-slot tier, self-hostable edition — see
WORK.md "What the private repo IS"). Research basis: direct reading of the
modeld source in the OSS runtime submodule (`runtime/modeld`,
`runtime/cmd/modeld`, 114 Go files) inside the private archive, plus current
web research on vLLM (2026) since a training-cutoff answer would be stale.

## modeld, from the source

**Shape:** single local owner, single active model, many persistent
sessions. A cross-process lease file (`owner` package) fences one daemon per
data root. Backends are direct cgo bindings — llama.cpp (GGUF, CPU/CUDA/HIP/
Metal) and OpenVINO (IR, CPU/GPU/NPU) — not a wrapped HTTP server. Transport
is a bespoke gRPC contract (`EnsurePrefix`/`PrefillSuffix`/`Decode`/`Embed`/
`Snapshot`/`Restore`), not OpenAI-compatible REST.

**Concurrency model, confirmed from `modeld/slot/service.go`:** every
operation acquires a single buffered channel (`op chan struct{}, 1`) before
touching the resident session — LoadModel, OpenSession, Embed, and every
Decode all serialize through the same one-at-a-time gate. There is no
batching, no paged attention, no concurrent multi-request path. This is not
an oversight; it is the stated design ("one active model, single-owner").

**What it does well (real, not aspirational):**
- **Device leasing** — cross-process ownership via a TTL'd lease file; a
  follower detects the holder and steps back cleanly.
- **Capacity planning** — `modeld/capacity`: resolves the *effective* context
  window from live free device memory and the model's per-layer KV-cache
  profile (windowed vs. global layers), not the model's trained ceiling.
  Recomputed live, not frozen at launch.
- **KV residency** — `modeld/residency`: backend-neutral hot/cold token-range
  policy (which ranges must stay resident, which may evict to a host-RAM
  cold store), plus session snapshot/restore with content-addressed blobs on
  disk. This is the stated differentiator in the archive's own
  `modeld-local-inference-landscape.md`: durable, reusable coding-agent
  context, not raw throughput.
- **Multi-backend** — llama.cpp and OpenVINO share one capacity/residency
  policy; darwin gets llama.cpp+Metal only (no OpenVINO on Apple Silicon).

**What it does NOT do:** continuous batching, paged-attention-style KV
sharing across requests, an OpenAI-compatible endpoint, or any concurrency
model beyond one owner talking to one session at a time. It is not
multi-tenant-serving-shaped by design, not by gap.

**Build/distribution cost:** cgo throughout; a Linux dev build needs
`gcc`/`g++`/`cmake`/CUDA toolkit (with a documented silent-CPU-fallback trap
if `nvcc` isn't on `PATH`); OpenVINO needs its own Python-wheel SDK **at build
time** (not at runtime — narrower than vLLM's Python dependency, see below).
Official releases use a two-role pipeline: per-OS dependency producers bundle
native libs to S3, a separate release-assembly step links and packages
`modeld`. Per the archive's own build docs: **Linux is verified end-to-end;
darwin and Windows native build chains are explicitly unverified/needs
porting.**

**Current state:** 123 Go files across `modeld/` + `cmd/modeld`, 67 of them
test files (54% — a maintained ratio, not a stub). 49 commits touching those
paths in the archive's history; last touched 2026-07-22, six days before this
survey. Actively maintained, not abandoned.

**License:** the home repo (`contenox/runtime`, OSS) is Apache-2.0, confirmed
from its `LICENSE` file.

## vLLM, from current research (2026)

**What it gives that modeld does not:** PagedAttention (non-contiguous KV
block allocation, cited memory-waste reduction of 55–80%), continuous
batching (packs concurrent requests into shared GPU passes), Flash Attention
+ CUDA graphs, a built-in OpenAI-compatible API server, tensor parallelism
for multi-GPU, and reported throughput of 14–24x over naive HuggingFace
Transformers serving on the same hardware. It has a real ecosystem: KServe
ships a first-class vLLM `ServingRuntime`/`LLMInferenceService` (v0.16+) with
KEDA-based GPU autoscaling on queue-depth/utilization metrics.

**What it costs:** GPU-focused in practice. Practical minimum for meaningful
concurrency on a 7B model is a 24GB card (RTX 3090/4090-class); FP8
quantization can shrink a 7B footprint to ~8GB on recent releases, but below
that tier the sources say "use something else." The CPU backend exists but
is explicitly secondary — "not where vLLM's best work lives" — with HTTP/
tokenization context-switching hurting cache efficiency on CPU specifically.
Native Windows support requires CUDA 13 and an RTX 6000 Ada-class GPU or
newer — not a consumer/self-host story on Windows. It is not embeddable: the
base Docker dependency layer (Python + PyTorch + CUDA) alone runs ~2GB, and
it is always a server process, never an in-process library. License:
Apache-2.0.

## Per-deliverable physics

- **Deliverable 1 (hosted free tier):** many concurrent, low-value requests
  — throughput and fairness dominate. modeld's single-serialized-operation
  model means one tenant's request blocks every other tenant on that
  instance; wrong shape regardless of maturity. vLLM's shape fits. Below
  roughly 15–20 concurrent users, a single `llama-server` instance (see Buy
  vs. build) is cheaper and simpler than standing up a vLLM stack — a
  legitimate stepping stone before vLLM is justified by traffic, consistent
  with WORK.md's own phase-1/phase-2 gateway plan.
- **Deliverable 2 (paid GPU-slot tier):** per-tenant isolation is the
  requirement, not shared multi-tenant serving inside one process. "GPU
  slot" already means one leased instance per tenant — architecturally
  closer to modeld's single-owner shape than deliverable 1's pooled-traffic
  shape. In practice this is still best served by vLLM-per-slot (a paying
  tenant's slot should still get batching/paged-attention benefits for their
  own concurrent tool-calling/agentic traffic), with the connectorruntime/
  vald-operator reconciler pattern doing the leasing — not a different
  engine, a different allocation policy.
- **Deliverable 3 (self-hostable edition):** one person's hardware, often
  CPU or a single consumer GPU, must install without a Python runtime
  bootstrap. This is precisely modeld's existing shape: no Python needed at
  runtime for the llama.cpp path, a working per-OS bundle+S3 release
  pipeline (Linux proven), and the residency/capacity differentiator is
  exactly what a single durable coding session on modest hardware wants.
  vLLM's GPU-only-in-practice reality and heavy Python/CUDA footprint make it
  a poor fit here regardless of license.

## License physics (2026, verified — not assumed)

None of the realistic candidates carry a GPL/AGPL/SSPL/BSL license. This
means license is **not** the differentiator for this decision — it rules
nothing in or out. Stated per candidate, per deliverable (1/2 = we operate,
never distribute; 3 = we distribute):

| Stack | License (verified) | D1 operate | D2 operate | D3 distribute |
|---|---|---|---|---|
| modeld (`contenox/runtime`) | Apache-2.0 | clean | clean | clean |
| vLLM | Apache-2.0 | clean | clean | clean (permissive, if ever bundled) |
| llama.cpp / `llama-server` | MIT | clean | clean | clean |
| Ollama | MIT | clean | clean | clean (as an operated component only — WORK.md already declines to recommend it to users) |
| LocalAI | MIT | clean | clean | clean |
| Hugging Face TGI | Apache-2.0 (reverted from a 2023 HFOIL detour; verified current) | clean | clean | clean — but project entered maintenance mode Dec 2025: momentum risk, not a license risk |
| Ray / Ray Serve | Apache-2.0 | clean | clean | n/a (orchestration only, not shipped to self-hosters) |
| KServe | Apache-2.0 | clean | clean | n/a (k8s-native, wrong shape for one machine) |
| LiteLLM (core proxy) | MIT (the `enterprise/` subtree is separately licensed; irrelevant here — we'd run the MIT core, not buy their enterprise tier) | clean | clean | clean |

Since GPL/AGPL/SSPL/BSL exposure was the one axis that could have forced a
decision by itself, and it didn't, this decision rests entirely on
capability/effort/ops-burden grounds below.

## Buy vs. build for the inference layer

The private repo is internal guts, not an OSS project we're obligated to
build ourselves — existing pieces are preferred wherever they fit. Evaluated
beyond the vLLM/modeld binary:

- **`llama-server` (llama.cpp's own server, MIT):** exposes `/v1/chat/
  completions` and friends, and genuinely supports continuous batching via
  `--parallel`/`--cont-batching` — real concurrent-request handling, not
  single-slot. It wraps the *same* llama.cpp core modeld binds to directly.
  For under ~15–20 concurrent users it beats standing up vLLM on cost and
  complexity. It does not expose modeld's KV hot/cold range eviction or
  snapshot/restore/branch API — those require the low-level calls modeld
  makes directly, not the HTTP surface. A real stepping-stone option for
  deliverable 1 at low traffic, not a modeld replacement for deliverable 3's
  residency story.
- **Ollama, operated (not recommended to users):** MIT, wraps llama.cpp,
  adds model-pull UX. Same wrong-shape problem as modeld for 1/2 — it is
  also single-model/low-concurrency oriented, just friendlier to manage.
  WORK.md already demotes Ollama to embeddings/index duty; no reason to
  promote it for hosted serving either.
- **LocalAI (MIT):** an OpenAI-compatible gateway across many backends and
  modalities — closer to "buy the whole self-hosted gateway" than a
  point solution. Fallback candidate for deliverable 3 if modeld's
  maintenance ever becomes unsustainable, but it sacrifices the residency
  differentiator entirely and is generically model-marketplace-shaped (per
  the archive's own comparison doc), not coding-agent-memory-shaped. Not a
  first choice today.
- **TGI:** Apache-2.0, but Hugging Face put it in maintenance mode in
  December 2025. Avoid betting build effort on a stalling project regardless
  of its clean license.
- **Ray Serve / KServe (Apache-2.0):** these are Kubernetes-native
  *orchestration*, not inference engines — they wrap vLLM/TGI/Triton as the
  runtime. KServe ships a ready vLLM `ServingRuntime` with continuous
  batching, paged attention, and KV-cache reuse pre-wired, plus KEDA GPU
  autoscaling on queue depth. This runs on exactly the k3s substrate the
  archive's Terraform already provisions. **Recommend evaluating KServe-on-
  k3s as the orchestration layer for deliverables 1/2**, replacing a real
  chunk of the hand-rolled "routing/queuing/quota/GPU-scheduling" effort the
  survey flagged as new weeks-of-work, rather than hand-rolling a scheduler
  on top of the connectorruntime/vald-operator pattern from scratch.
- **LiteLLM (MIT core):** an AI gateway/proxy — virtual keys, per-key/team
  budget tracking with resets, RPM/TPM rate limiting, load balancing and
  fallback across 100+ providers including vLLM. This maps almost exactly
  onto WORK.md's own phase-1 gateway plan (token auth, per-install rate
  limits, hard monthly cap, kill switch) and the "envelope as a transparent
  monthly spend budget" pricing language. **Recommend adopting LiteLLM's OSS
  proxy as the gateway layer** in front of whichever engine serves a given
  tenant, instead of hand-writing token-auth/budget/rate-limit code — it
  needs only a Postgres/Redis backing store, which the stack already has.

### Reframing modeld's value

Split modeld's value into two tiers, because they have different buy-vs-
build answers:

- **Tier A — cheap to replicate without cgo:** the cross-process lease file
  and the capacity/memory-budget policy computed before launch. Could be
  rebuilt as a thin supervisor wrapping `llama-server`/vLLM processes.
- **Tier B — NOT separable from the current cgo depth:** the KV-residency
  hot/cold/snapshot/branch logic, implemented via direct llama.cpp/OpenVINO
  C-API calls (`llamacppshim`, `ovsession`), not through any server's HTTP
  surface. This is modeld's actual stated differentiator, not incidental
  plumbing — no candidate above (`llama-server`, Ollama, LocalAI, vLLM)
  exposes an equivalent.

**Honest verdict:** "keep only the leasing idea and wrap an existing engine"
is possible, but it means deliberately discarding the residency
differentiator, not merely trimming incidental maintenance — the plumbing
*is* the differentiator. Given modeld already exists, is meaningfully tested
(67/123 files), is actively maintained, and has a working Linux-verified
release pipeline, abandoning it to save maintenance on code that already
ships is not the efficient move here. The efficient move is the opposite of
what "prefer existing pieces" might suggest at first glance: keep modeld
whole for deliverable 3, and spend the "buy existing pieces" discipline on
the *surrounding* layer instead — KServe for orchestration, LiteLLM for the
gateway — where hand-rolling was actually planned.

## Recommendation

- **Deliverable 1 (free tier):** vLLM as the eventual engine, `llama-server`
  as a legitimate cheaper stepping stone under real low-traffic conditions,
  both behind a LiteLLM gateway (auth/budget/rate-limit), orchestrated via
  KServe-on-k3s where ready. Not modeld — wrong concurrency shape by design,
  not by gap.
- **Deliverable 2 (paid GPU-slot tier):** same engine choice as deliverable
  1 (vLLM per leased slot), using the connectorruntime/vald-operator
  reconciler pattern as the GPU-slot-lease mechanism. "GPU slot" is a
  capacity/isolation policy, not a different inference engine.
- **Deliverable 3 (self-hostable edition):** modeld, as-is. Already the
  right shape (one machine, CPU/consumer GPU, no runtime Python for the
  llama.cpp path, existing per-OS release pipeline, Linux proven), and its
  residency/capacity/leasing differentiator is real, built, tested product
  value — not a placeholder to discard for maintenance convenience.
- **"Both" is coherent, not indecision:** modeld and vLLM answer different
  physics (one machine vs. many tenants), not the same job done twice. That
  reading would only be indecision if one deliverable used both
  interchangeably without a reason — that isn't the design here.

## Integration seam if both

Contenox already has a `modelrepo.Provider` interface implemented
independently by an HTTP/OpenAI-compatible `vllm` provider
(`internal/models/modelrepo/vllm/` in this harness repo) and, via the OSS
runtime's gRPC-based modeld transport contract, a local-backend provider.
**The interchangeability the question assumes is partly already solved, just
not the way it's usually framed:** contenox's Provider abstraction already
treats vLLM's HTTP/OpenAI wire format and modeld's bespoke gRPC wire format
as two implementations of one interface. modeld does not need to grow an
OpenAI-compatible REST shim for the *client side* to be interchangeable —
that already works, which is exactly how WORK.md's "phase 2 swaps the
backend to a self-hosted vLLM box... no user notices the swap" is already
true today. What still needs building for the hosted tiers is provider
*selection/routing* at the gateway (LiteLLM, or a thin policy layer) that
picks the right backend per tenant/plan — a routing problem, not a
compatibility-shim problem. An OpenAI-compatible shim on modeld would only
matter if something outside contenox's own Provider abstraction needs to
talk to it directly (third-party OpenAI-compatible tooling) — a "nice if the
free tier ever needs it" item, not a blocker.

## Effort classes

- **Deliverable 1 MVP:** days for the gateway (LiteLLM config + the
  Stripe/budget wiring that partly exists already); weeks for the vLLM
  swap-in once traffic justifies it — matches WORK.md's existing phase-1/
  phase-2 plan; this decision confirms vLLM as the phase-2 backend rather
  than leaving it open.
- **Deliverable 2:** weeks — per-tenant GPU scheduling/isolation is new
  regardless of engine choice (KServe narrows but doesn't eliminate this);
  billing/metering/webhooks still need building (see ee-inventory.md gaps).
- **Deliverable 3:** days–weeks to *finish* what exists (the darwin/Windows
  native build chain is explicitly unverified per the archive's own build
  docs), not build-from-zero.

## Top 3 risks

1. **vLLM has no good CPU story.** Deliverables 1/2 therefore have zero
   cheap tier without a GPU budget. Mitigation already decided in WORK.md:
   proxy paid Gemini Flash first, swap to a self-hosted vLLM box only once
   traffic justifies the spend.
2. **modeld's darwin/Windows distribution is unverified.** Deliverable 3's
   "runnable by anyone" promise is proven on Linux only today; OpenVINO
   isn't available on Apple Silicon at all, and the Windows native build
   chain is flagged unverified in the archive's own docs. Overpromising
   deliverable 3 before this closes is a real risk.
3. **Two-engine maintenance tax.** Running vLLM (Python/PyTorch/CUDA) and
   modeld (Go/cgo/native) is two build/ops surfaces, not one. Mitigated by
   the deliverables being genuinely disjoint (no single traffic path
   operates both at once), but real and worth naming so it isn't a later
   surprise.

## What would change this decision

- vLLM CPU parity maturing (current signals say it stays secondary) would
  weaken the "GPU-only in practice" risk and let vLLM absorb more of
  deliverable 3's low end.
- If modeld's residency/snapshot differentiator is measured to not matter
  commercially (no user-visible lift), the Tier-A/Tier-B split above
  changes: retiring modeld's cgo depth in favor of `llama-server`/LocalAI,
  keeping only the lease/capacity-supervisor idea, becomes reasonable.
- If Windows/macOS demand dominates the self-host audience and the native
  build chain doesn't get finished, the deliverable-3 stopgap could shift to
  LocalAI/`llama-server` (both ship cross-platform prebuilt binaries today)
  even if modeld's differentiator is judged worth keeping in principle.
- If KServe/LiteLLM operational cost exceeds the solo-maintainer bootstrap
  budget (k8s complexity tax), the "buy" call on the orchestration layer
  would need to shrink back toward the hand-rolled connectorruntime pattern
  already in the repo.

## Sources (vLLM/ecosystem claims, 2026)

- [What is vLLM? The High-Throughput LLM Serving Engine in 2026](https://futureagi.com/blog/what-is-vllm-2026/)
- [vLLM Throughput Guide - PagedAttention and Batching Tips (2026)](https://blog.easecloud.io/ai-cloud/increase-throughput-with-vllm-serving/)
- [vLLM Hardware Requirements & GPU Guide | VRLA Tech](https://vrlatech.com/vrla-tech-workstations/vllm/)
- [Running vLLM on Your Own Hardware: The Production Guide for 2026 - VRLA Tech](https://vrlatech.com/running-vllm-on-your-own-hardware-the-production-guide-for-2026/)
- [Inside vLLM's CPU backend: a new contributor's notes](https://dev.to/daniel_lm_bf221f14807a4fd/inside-vllms-cpu-backend-a-new-contributors-notes-4kin)
- [CPU - vLLM Documentation](https://docs.vllm.ai/en/stable/getting_started/installation/cpu/)
- [AI/ML on Kubernetes 2026: Production Stack Guide (vLLM, Kueue, KServe, Ray)](https://kubernetesguru.com/ai-ml-on-kubernetes-2026-stack-guide/)
- [Autoscaler | KServe](https://kserve.github.io/website/docs/model-serving/generative-inference/autoscaling)
- [llama.cpp server README (ggml-org)](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md)
- [Text-Generation-Inference: Revert license to Apache 2.0 (#1714)](https://github.com/huggingface/text-generation-inference/commit/ff42d33e9944832a19171967d2edd6c292bdb2d6)
- [litellm · GitHub (BerriAI)](https://github.com/BerriAI/litellm)
- [Budgets, Rate Limits | liteLLM](https://docs.litellm.ai/docs/proxy/users)
