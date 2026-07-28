# EE buy-vs-build: supporting machinery, 2026-07-28

Companion to [ee-inventory.md](ee-inventory.md) and
[ee-hosting-foundation.md](ee-hosting-foundation.md) (read those first for
what exists; this doc does not repeat their per-file detail except where the
buy/build call needs a number). Read-only pass over the private archive —
nothing modified. Scope: the SUPPORTING MACHINERY around the three-deliverable
inference stack (free tier, paid tier, self-hostable edition) — not the
inference engine itself (see the companion inference-stack-decision doc).

**Frame:** that repo is private internal guts, not OSS — buy off-the-shelf
wherever it fits instead of building and maintaining our own. A solo
maintainer's scarcest resource is attention; every hand-rolled subsystem still
standing after this pass is a deliberate, permanent tax, not an oversight.

**License discipline applied per area:** deliverables 1 & 2 (we RUN it,
never distribute) — plain GPL doesn't trigger (no distribution occurs), but
AGPL DOES need scrutiny even here (its network-use clause is built to close
exactly the SaaS loophole GPL leaves open), and SSPL/BSL specifically target
hosted-service offerings (SSPL demands open-sourcing the entire stack used to
offer the service; BSL restricts commercial/hosted use until its change
date) — so AGPL/SSPL/BSL all get flagged for hosting, not just GPL's
distribution trigger. Deliverable 3 (we DISTRIBUTE a self-hostable edition) —
GPL/AGPL/SSPL/BSL all become live legal concerns the moment a copy goes to an
end user; that edition wants permissively-licensed (Apache-2.0/MIT/BSD)
components throughout, full stop. Flagged explicitly below wherever a
candidate is workable for hosting but poisonous for the shippable edition —
and separately wherever AGPL/SSPL/BSL raise the bar even for hosting-only use.

---

## 1. Identity & tenancy

**Current:** `bob2/internal/auth` (15 files / ~1.9K LOC) — Postgres-backed
cookie sessions, JWT-ish tokens (`claims.go`/`tokens.go`), password auth +
password reset, a pluggable `LoginProvider` interface with only `"password"`
concretely implemented (OAuth/SAML providers are stubbed, never built). Roles
are `superadmin` / `tenant_owner` / `tenant_admin` / `tenant_user`
(`store/users.go`), tenant-scoped invites (`store/invites.go`). Site side:
`site/lib/platform/auth.ts` + `invites.ts` (~500 LOC) plus
login/signup/forgot-password/reset-password/invite pages (~614 LOC). Total
~3K hand-rolled LOC. 51 in-repo importers of `auth` alone — the single
most load-bearing package in the tree.

**Off-the-shelf, verified 2026:**

| Project | License | Maintenance | Self-host weight | Multi-tenant fit | Embeddability |
|---|---|---|---|---|---|
| Keycloak | Apache-2.0, unchanged; CNCF Incubating | Active, Red Hat + CNCF, quarterly releases | JVM + own Postgres, 2+ containers, 2-4GB RAM, own admin UI | Native "Organizations" (stable since v26) | Mostly hosted/redirect login; headless mode discouraged — fights the existing custom Next.js UI |
| Ory Kratos+Hydra | Apache-2.0 core | Active, well-funded | 1 Go binary + Postgres, BYO UI — lightest "hosted-IdP" option | **OSS Kratos is explicitly single-tenant** — multi-tenancy is paywalled (Ory Enterprise License) | Best headless API design of the five |
| Authentik | MIT core + proprietary enterprise tier | Active, cadence slowing (2mo→3mo in 2026) | 3 containers since 2025.10 | "Tenants" = separate instances/schemas per tenant — wrong shape (not in-app workspace roles) | Hosted-IdP style, more integration friction |
| Zitadel | **Apache-2.0 → AGPLv3 for new code, Mar 2025** (old code stays Apache; mixed codebase); commercial license available | Active, steady releases | Lightest full platform: 1 Go binary + Postgres, ~512MB-1GB | Best native fit: Organizations/Projects/project-grants | Hosted login by default, full Session API for custom UI |
| SuperTokens | Apache-2.0 core throughout, no changes | Active, smallest team (~6-10 people, YC-backed) — highest bus-factor risk | Lightest overall: 1 Core container + Postgres, SDK runs in-process | Multi-tenancy/per-tenant roles are **free OSS features** | API/SDK-first — closest architectural match to what exists today |

**Recommendation: adopt SuperTokens.** Zitadel has the best multi-tenancy
model and lightest footprint, but the AGPLv3 flip is exactly the trap this
assessment watches for — under our own license-discipline rule it needs legal
scrutiny even for hosted-only use (deliverables 1&2, network-use clause), and
is outright **poisonous for deliverable 3** without buying a commercial
license. Ory's multi-tenancy is paywalled (the one feature this product needs
most). Keycloak's JVM weight
and redirect-oriented UX fight the install-weight goal for a self-hostable
edition. Authentik's tenant model is the wrong shape entirely. SuperTokens is
Apache-2.0 across the board (clean for all three deliverables, which matters
because identity is the one area that must ship in the self-hostable edition
too), needs only one extra container, and is API/SDK-first enough that the
existing ~1.1K LOC of Next.js auth pages can keep their shape while calling
the SDK instead of hand-rolled session/JWT/reset logic. It also unlocks the
stubbed OAuth/SAML provider slots for free. Keep the tenant/role/invite domain
model (`store/users.go`, `invites.go`) as-is — that's product authorization
logic, not identity plumbing, and SuperTokens doesn't replace it.

**Effort:** 2-3 weeks — one new container + its Postgres tables, migrate
~1.9K LOC of Go session/JWT/reset code and ~1.1K LOC of Next.js pages to call
the SDK, re-verify the 51-importer blast radius.

---

## 2. Metering & usage accounting

**Current: does not exist.** No usage-event pipeline, no token/GPU-slot
counter, no aggregation, no rating engine anywhere in the archive.
`site/middleware/rateLimit.ts` (~70 LOC) is a request-count throttle, not a
usage ledger — it doesn't persist billable quantities. This is new code
regardless of the buy-vs-build call, and it is the highest-stakes area in
this whole assessment: billing correctness is unforgiving.

**Off-the-shelf, verified 2026:**

| Candidate | License | Status | Built for LLM tokens? | Self-host weight |
|---|---|---|---|---|
| OpenMeter | Apache-2.0 | **Acquired by Kong, Sept 2025**, folding into commercial "Konnect Metering & Billing"; OSS promise stated as holding short-term but roadmap now depends on Kong | Yes — Stripe sync, LiteLLM/OTel collectors, AI cost meters | Heavy: Kafka/Redpanda + ClickHouse + Kafka Connect + Postgres |
| Lago | **AGPLv3** (deliberate, reaffirmed publicly) | Active, real adoption (Mistral AI uses it for usage billing) | Yes, directly — ships an "Agent SDK" that wraps LLM clients and extracts token dimensions | Moderate: Rails + Postgres + Redis/Sidekiq |
| Kong/APISIX rate-limit plugins | Kong core Apache-ish; APISIX Apache-2.0; Kong's AI-cost plugin is Enterprise-only | Mature as throttles | No (generic) — Kong's token-cost-aware plugin exists but is paywalled; APISIX's AI rate-limit plugins are free but still limiters | Light-moderate |
| LiteLLM proxy (built-in tracking) | MIT | Mature, widely used | Yes, natively — per-key/team spend, `spend_tracked` webhook, OpenMeter integration | Light: proxy + own Postgres tables |
| Stripe Billing Meters | N/A (SaaS) | Materially overhauled 2026 (legacy Usage Records API retired); **Stripe acquired Metronome Feb 2026** (the engine OpenAI/Anthropic metered on), folding it into Billing | Generic sum/count — works if you compute the unit yourself | N/A (hosted) |

Kong/APISIX limiters produce in-window decrementing counters, not an
append-only replayable ledger — fine for kill-switches, not billing-grade (no
dispute/adjustment trail). Stripe Meters are explicitly not a
source-of-truth ledger: 35-day/5-minute backfill window, ~48h late-aggregation
cutoff, non-guaranteed ~1hr invoice-finalization grace, ~24h idempotency
dedup window.

**Recommendation: hybrid — build the thin ledger, buy the rating.**
Homegrown append-only usage-event ledger (Postgres, one row per rated unit —
tokens in/out, GPU-seconds, idempotency-keyed) emitted where tokens are
counted in our own gateway; feed rated aggregates into Stripe's native Meter
Events for invoicing. Do not adopt OpenMeter (ops weight + acquisition
uncertainty now that it's a Kong product) or Lago as the system of record —
**AGPL blocks deliverable 3 outright** if bundled, and under our own
license-discipline rule it also needs legal scrutiny for hosted-only use
(deliverables 1&2, network-use clause), on top of being a second stateful
service for no capability gain at our pricing complexity. Revisit Lago's
hosted/cloud offering later, hosted-tiers-only, only if pricing logic
(tiers/credits/proration) outgrows hand-rolling and the AGPL question gets a
deliberate legal look first.

**Effort:** the ledger is genuinely new code regardless of the buy call —
event schema + idempotent writer + Stripe Meter Events wiring, roughly 1-2
weeks. "Buy" here means Stripe Meters covers the invoicing math so we don't
also build a rating engine; it does not shrink the ledger itself.

---

## 3. Billing & subscriptions

**Current:** one-time, manually-reviewed Stripe Checkout only
(`site/STRIPE_SETUP.md`, `site/app/api/checkout`, ~76 LOC) for a single
"Pilot Workspace" product. No webhooks — an operator manually advances orders
after checking the Stripe dashboard. No subscription lifecycle, no recurring
billing, no usage-based line items, no public self-serve catalog.
`site/app/bob/billing` (~258 LOC) matches this pilot-only state.

**Off-the-shelf, verified 2026:**

| Option | Usage-based billing? | Seller of record | VAT burden left on founder | Take rate |
|---|---|---|---|---|
| Stripe Billing + Tax (Basic) | Yes, mature (Metronome-absorbed engine) | Founder | Full — calc+collect only; founder registers/monitors/files | ~0.7% + 0.5% tax |
| Stripe Billing + Tax (Complete) | Yes | Founder | Partial — filing partners automate returns, founder still legally liable | ~0.7% + tax add-on |
| Stripe Managed Payments | Immature for recurring/metered, ~35 countries live | **Stripe** | None | ~6.4%+ effective |
| Lago (self-hosted) | Yes, core strength | Founder | Calculates VAT + VIES B2B reverse-charge; **files nothing** | Free (AGPLv3) |
| Kill Bill (self-hosted) | Yes, via plugins (heavier lift) | Founder | Calc-only via AvaTax/Vertex plugin; filing external | Free (Java+DB+plugin ops burden) |
| Paddle (MoR) | Yes, confirmed | **Paddle** | None — full global reg/filing/remittance | ~5% + $0.50/txn |
| Lemon Squeezy (MoR; Stripe-owned since 2024, still standalone 2026) | Yes | **Lemon Squeezy/Stripe** | None | 5.5-11% effective |

Stripe Tax is **not** a Merchant of Record — the founder stays the legal
seller and remains liable for VAT registration/filing even on the automated
filing add-on. Only true MoR platforms (Paddle, Lemon Squeezy, or the still
immature Stripe Managed Payments) fully remove that burden, at a real cost
premium. German GoBD 10-year invoice retention and the ~€25,000
Kleinunternehmer threshold apply regardless of platform choice — no billing
tool solves those, only bookkeeping discipline. The EU OSS scheme (one
quarterly return for all B2C EU sales) is the biggest lever once
VAT-registered.

**Recommendation: hybrid, split by deliverable.**
- **Deliverable 2** (hosted paid tier, metered token/GPU pricing, presumably
  higher volume) → Stripe Billing + Stripe Tax, self-filed initially, upgrade
  to the Complete filing add-on once volume justifies it. Lowest take rate
  (~0.7-1.2% vs 5-11%) matters most here. Skip Lago/Kill Bill — self-hosting a
  billing engine adds ops burden without removing any filing obligation
  Stripe doesn't already reduce.
- **Deliverable 3** (self-hosted edition, one-time license or simple
  subscription, presumably lower volume/simpler pricing) → Paddle or Lemon
  Squeezy as MoR. Higher take rate is a small absolute cost at low volume, and
  it fully offloads VAT registration/filing/remittance for that stream —
  exactly where a solo founder should buy down compliance hours rather than
  optimize basis points.
- Net effect: the founder carries direct VAT filing responsibility only for
  the Stripe-billed hosted-paid stream, collapsed to one quarterly EU return
  via OSS enrollment.

**Effort:** replacing pilot-checkout with real Stripe subscriptions +
webhooks + the Meter Events wiring from area 2 is weeks (already flagged as
"real work, not starting from zero" in the prior survey); standing up
Paddle/Lemon Squeezy for deliverable 3 is comparatively fast (days) given
simpler pricing there.

---

## 4. API gateway / quota / abuse control

**Current:** `site/middleware/rateLimit.ts` (~70 LOC) — IP-hash +
namespace, Postgres sliding-window request-count limiter. No per-key
budgets, no token-aware limiting, no model routing, no cost-based throttling.
Judged specifically against an LLM-token workload, not generic HTTP traffic.

**Off-the-shelf, verified 2026:**

| Candidate | License | LLM-token-aware natively? | Self-host weight | Gateway+quota in one? |
|---|---|---|---|---|
| Kong OSS + Konnect | Apache-2.0 core; Konnect/Enterprise proprietary | Routing = OSS; token budgets (`AI Rate Limiting Advanced`) + semantic cache = **Enterprise-only** (~$30-50k+/yr) | Medium — Nginx, Postgres needed in DB mode for multi-node counters | No — the quota half is paywalled |
| Apache APISIX | Apache-2.0 (ASF), fully permissive | Yes — `ai-proxy`/`ai-rate-limiting`/`ai-prompt-guard`/`ai-cache` all OSS in-tree | **Light** — Standalone Mode (no etcd) runs as one process off a local config | Yes, fully OSS |
| Traefik | MIT core; all AI-gateway features live in **Traefik Hub** (commercial, license-token metered) | No — LLM features are entirely paid-tier | Core is light; Hub is a separate paid product | No — not distributable OSS |
| Envoy (+ Envoy AI Gateway, v1.0 June 2026) | Apache-2.0 throughout | Yes — native per-user/model/team token rate limiting via the Global Rate Limit API | **Heavy** — K8s + Gateway API CRDs + xDS control plane + typically a Redis-backed limiter service | Yes, but infra cost is prohibitive at this scale |
| LiteLLM proxy | Core **MIT**; a carved-out `enterprise/` dir is proprietary/BSL-style (gates SSO >5 users, audit-log retention, some guardrail-vendor integrations — nothing this product needs yet) | Yes, purpose-built — virtual keys, per-key/team/org budgets in tokens **and** dollars, real-time spend tracking, multi-provider routing/fallback | Medium-light — proxy + Postgres, optional Redis only for multi-replica budget consistency | Yes, by design — the only candidate modeled around LLM token/dollar economics, not generic HTTP |

Kong, Traefik, and (practically) Envoy are each disqualified for a different
reason — Kong and Traefik gate the exact capability needed (token
budgets/semantic cache) behind a commercial tier, Envoy is licensed cleanly
but demands a Kubernetes platform-team footprint this scale doesn't have.
APISIX and LiteLLM are the two genuinely viable, fully-OSS options; they
solve different halves of the problem (APISIX = generic gateway with
token-aware plugins bolted on; LiteLLM = purpose-built LLM proxy that does
routing and budget enforcement as one job).

**Recommendation: hybrid, split by install-weight, not license** — LiteLLM's
core is MIT throughout, so unlike identity/metering this split is not a
copyleft trap; it's an operational-weight call.
- **Hosted free + paid (1, 2): adopt LiteLLM.** One MIT-licensed service
  replaces both the routing job and the quota-enforcement job — per-key/team
  token-and-dollar budgets, real-time spend, hard caps, multi-provider
  fallback. Caveat: LiteLLM assumes an already-authenticated virtual key: it
  does not do pre-auth IP-abuse throttling. Keep the existing
  `rateLimit.ts` middleware in front of it as the free tier's first line of
  defense (the funnel mouth, most exposed to abuse); LiteLLM supplies the
  hard spend cap/kill switch behind that.
- **Self-hostable edition (3): don't bundle a second heavy service.** Extend
  the existing ~70-line middleware to parse token usage out of provider
  responses and add per-key budget columns to the row it already writes —
  this reuses the Postgres self-hosters already run instead of standing up
  LiteLLM's own stack for a single-tenant install. If a ready-made
  token-aware plugin is preferred over hand-rolling more middleware, Apache
  APISIX in Standalone Mode (Apache-2.0, no etcd) is the only fully-permissive
  option with native token-awareness and a genuinely light footprint — at the
  cost of one extra binary in the shippable edition.

**Effort:** hosted-tier LiteLLM adoption (stand up proxy + Postgres, issue
virtual keys per tenant, migrate model-routing config, keep the IP-hash
middleware in front) is roughly 1-2 weeks. Extending `rateLimit.ts` for the
self-hostable edition is days — incremental to existing code, not a new
platform.

---

## 5. Provisioning & delivery

**Current (all confirmed real, per ee-inventory.md / ee-hosting-foundation.md):**
- `deploy/gcp` — Terraform (VPC, 2-VM k3s bootstrap: gateway + workload node,
  Artifact Registry) + rendered k8s manifests + bootstrap scripts. Lean, DIY
  2-VM k3s (not managed GKE) — appropriately scaled for a solo operator.
- `bob2/internal/connectorruntime` (6 files / 1.78K LOC) — the reconciler:
  watches control-plane Postgres rows and reconciles k8s Deployments/Services
  to match. Standard controller/reconciler pattern, not a reinvention of one.
- `bob2/internal/appcatalog` + `apprelease` + `appsource` (~2.4K LOC
  combined) — OCI Helm chart → hosted-instance catalog, worked example
  `apps/minio`.
- Two CI/CD pipelines — `.github/workflows/{ci,deploy,deploy-website,
  mirror-images,connectors,infra-apply,infra-plan,infra-unlock-state}.yml`:
  app deploy (Next.js/Go → k3s over IAP) and the separate contenox.com
  static-site deploy from the OSS submodule.

**Judgment: genuinely differentiated, not reinvented k8s primitives.** The
Terraform+k3s bootstrap is a standard, appropriately lightweight IaC pattern
(2 VMs, not a hand-rolled scheduler) — a managed k8s (GKE/EKS) would cost
more, not less, at this scale. The reconciler is built *on top of* the
standard controller pattern to do something no off-the-shelf product does out
of the box: map this product's own control-plane rows (tenant/plan state) to
per-tenant k8s objects — that mapping is inherently product-specific business
logic, the same shape as writing a Kubernetes Operator, which is the accepted
way to build this, not a shortcut around it. The one piece worth a comparison
check is the app-catalog: it overlaps conceptually with GitOps tooling
(ArgoCD/Flux + ApplicationSets) or ISV-delivery platforms like
Replicated/KOTS. Replicated specifically targets deliverable 3's exact shape
(licensed, air-gapped self-hosted delivery with license-key enforcement) —
but it's a paid vendor product, and the current app-catalog is already-built,
already-tested, 11-importer-deep code. Adopting Replicated would trade a sunk
asset for a new subscription and a new integration surface with no clear
capability gap it closes today. Worth a revisit only if deliverable 3 later
needs license-key enforcement or air-gapped installs specifically.

**Recommendation: keep, all of it.** No adopt-X here — this is the one area
where the archive is ahead of what buying would get, at zero incremental
license risk (Terraform/k3s are Apache-adjacent, own code otherwise). Note
(carried from ee-inventory.md, not new here): `vald-operator/` (1.6K LOC) is
confirmed zero live dependency — kept only as an operator-pattern reference,
separate from this "keep" call.

**Effort:** zero. Don't touch it.

---

## 6. Observability

**Current: missing entirely.** Zero hits for prometheus/grafana/otel/loki/
jaeger anywhere in the archive (grepped across `.go`/`.yaml`/`.yml`/`.ts`/
`.tf`) outside the OSS runtime submodule, which is out of scope. Already
flagged as gap #5 in the prior survey.

**Off-the-shelf, verified 2026:**
- **Grafana Cloud free tier** — 10k metric series, 50GB each logs/traces/
  profiles, 14-day retention, 3 users, $0 forever, no auto-upgrade. Zero ops
  burden; covers metrics+logs+traces from day one via a single OTel Collector
  instrumentation point.
- **Self-hosted LGTM** (Prometheus/Mimir + Loki + Tempo + Grafana +
  Alertmanager) — fits one 4GB VPS at this scale; the real cost is
  engineer-hours (retention tuning, compaction, capacity planning), not the
  software.
- **All-in-one alternatives** (SigNoz, OpenObserve) replace the whole LGTM
  stack with one OTel-native product — worth a look only if the multi-binary
  LGTM stack itself becomes the pain point.
- **Paging: Grafana OnCall (OSS) is archived as of March 2026** (maintenance
  mode Mar 2025 → archived Mar 2026) — the free self-hosted paging path is
  gone. Successor is Grafana Cloud IRM (hosted, paid above free-tier limits).
  Simpler for a solo operator: Grafana Cloud alerting → a lightweight push
  channel (ntfy, free/self-hostable; or Pushover, ~$5 one-time/platform)
  rather than a full incident-management platform.

**Recommendation: adopt Grafana Cloud free tier + ntfy or Pushover for
paging.** Minimum viable for a solo operator with a pager: instrument the
gateway/API once via OTel SDK, ship to Grafana Cloud's free tier, alert rules
→ webhook to ntfy/Pushover for phone-buzz alerting. No new stateful infra,
no license risk (hosted, or permissive/self-hostable for ntfy), an easy
upgrade path to self-hosted LGTM later if usage outgrows the free caps (same
query language either way).

**Effort:** days — the cheapest area to close of the six; the existing
deploy manifests already have room for an OTel Collector sidecar/DaemonSet.

---

## Summary

| Area | Current | Recommendation | Effort | License-clean for |
|---|---|---|---|---|
| Identity & tenancy | ~3K LOC hand-rolled (sessions/JWT/invites/roles) | Adopt SuperTokens (Apache-2.0) | 2-3 weeks | 1, 2, **and 3** |
| Metering & usage accounting | None | Hybrid: homegrown ledger + Stripe Meters | 1-2 weeks (unavoidable) | 1, 2 (own ledger only — no AGPL dep); n/a for 3 until self-host metering is scoped |
| Billing & subscriptions | One-time manual Stripe checkout only | Hybrid: Stripe Billing+Tax (deliverable 2) / Paddle or Lemon Squeezy MoR (deliverable 3) | Weeks (2) / days (3) | 1, 2, 3 (each leg is its own SaaS dependency, not bundled code — no distribution license issue) |
| API gateway / quota / abuse control | `rateLimit.ts` only (~70 LOC) | Hybrid: LiteLLM (deliverables 1,2) / extend own middleware (deliverable 3) | 1-2 weeks (1,2) / days (3) | 1, 2, 3 (LiteLLM core is MIT — split is weight-driven, not license-driven) |
| Provisioning & delivery | Mature Terraform+k3s, reconciler, app-catalog, 2 CI/CD pipelines | Keep, all of it | Zero | 1, 2, 3 (own code + permissive infra tools) |
| Observability | None | Grafana Cloud free tier + ntfy/Pushover | Days | 1, 2 (hosted only — deliverable 3's self-monitoring story is separate, not scoped here) |

## What we should stop maintaining

- Hand-rolled sessions/JWT/password-reset in `bob2/internal/auth` +
  `site/lib/platform/auth.ts` — once SuperTokens lands, this ~1.9K+~500 LOC
  of identity plumbing goes away (tenant/role/invite domain logic stays).
- The stubbed `LoginProvider` OAuth/SAML slots — never built; SuperTokens
  ships these, so there's nothing to finish here, just delete the stub
  interface once migrated.
- Manual-review Stripe checkout flow (`site/app/api/checkout`,
  `STRIPE_SETUP.md`'s "operator advances orders by hand" process) — replaced
  by real subscription webhooks.
- Building a custom token-budget/model-routing layer from scratch for the
  hosted tiers — LiteLLM already does virtual keys, per-key/team
  token-and-dollar budgets, spend tracking, and multi-provider fallback as
  one MIT-licensed job; there is no reason to hand-roll that twice.

## Decisions for the maintainer

1. **Identity migration timing** — adopt SuperTokens now (before paid-tier
   billing work starts, since billing needs stable per-tenant identity to
   key off of) or after V1 ships, accepting the hand-rolled auth a while
   longer? SuperTokens is Apache-2.0-clean for all three deliverables either
   way, so this is a sequencing question, not a license question.
2. **Metering ledger ownership** — build the homegrown usage-event ledger as
   part of the gateway work (area 4) or as a standalone service from day
   one? This affects whether deliverable 3's self-hosted edition gets any
   usage/quota visibility at all, or only the hosted tiers do.
3. **Billing split approval** — confirm the Stripe-for-hosted /
   Paddle-or-Lemon-Squeezy-for-self-hosted split, since it means running two
   separate payment relationships (two dashboards, two reconciliation
   processes) instead of one. If that operational split is unacceptable for
   a solo founder, the fallback is Stripe everywhere and carrying full VAT
   filing burden on deliverable 3's revenue too.

Pointer: see [README.md](README.md) for the full internal-docs index.
