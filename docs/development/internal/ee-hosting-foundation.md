# EE monorepo as the inference-product foundation — survey, 2026-07-28

Read-only survey of the private archive (nothing modified, nothing deployed).

**Corrected framing (2026-07-28):** the private repo IS the LLM inference
product — three deliverables on one stack: (1) a hosted free tier for OSS
contenox users, (2) a paid tier priced by tokens/GPU slots, (3) a
self-hostable edition of that same inference stack. Open-core line: pay for
capacity, never capability — the harness stays free forever, the inference
stack is the product AND is self-hostable. Everything else this survey
touches (tenancy, provisioning, billing, pipelines, rate limits) is
SUPPORTING MACHINERY for those three, not the product itself. "Host the
gateway, not the model" is a phase-1 build sequence for deliverable 1, not the
product definition — a prior pass at this doc conflated the two. Full
definition: WORK.md, "What the private repo IS."

Purpose of this survey: decide what the private repo contributes to that
three-deliverable stack, and what gets stripped. The findings below
(runnability, screenshots, keep/wipe reasoning, the modeld nuance) are
unchanged from the first pass — only the framing and the gap ranking were
wrong.

## Runs today on this box

`make dev` (docker compose) brings up the whole stack in about a minute —
registry, postgres, mailpit, minio, bob, site — with generated secrets already
in `.env` and no external credentials needed. Both prize UIs come up.

**One real bug to fix when the repo is next touched:** `compose.yaml`'s `site`
service mounts `./site` and `./runtime` but not `./packages`, while the site's
config aliases `@contenox/ui` and `@contenox/blueprint-renderer` into
`../packages/*/dist`. The homepage 500s without it. One added mount line
(`./packages:/packages:ro`) fixes it; the survey worked around it with an
override file rather than editing the repo.

k8s: local single-node k3s confirmed (context `default`, node `nox16`), but
nothing was deployed — compose covered both UIs, and the only k8s-specific
pieces have no UI of their own. Exercising the k8s path would need the external
vald-helm-operator + `ValdRelease` CRD installed cluster-wide, plus a kubeconfig
registered against bob's worker-cluster API. Neither yields a screenshot.

GCP Terraform path (`deploy/gcp/`) needs a real project/IAM/DNS — not runnable
locally, but it is the mature provisioning engine.

## Screenshots harvested

18 shots in the session scratchpad under `ee-harvest/` (authenticated views
captured by driving headless Chrome over CDP with real session cookies from a
throwaway signup). Publishable: site home, signup, operator login, bob
dashboard / files / search / connectors (Dummy Commerce + S3 cards) / members /
beam workbench, operator console tabs (overview, workspaces, worker pools,
hosted apps), MinIO login, Mailpit.

**Not for publication:** `admin-dashboard.png` — the operations page shows a
"recent checkout orders" card with an email-shaped address and an order record
(synthetic dev seed, but record-shaped). Dropped: an analytics 404 and a raw
registry JSON response.

These fill the Lab's missing captures (`rnd-assets.md` lists `beam-login.png`,
`beam-new-chat.png`, `modeld-console.png`, `ui-library-storybook.png` as still
needed — the bob/site shots cover the Bob page and then some).

## Keep / wipe

(Pillar labels below match the revised four-pillar scheme in
[ee-inventory.md](ee-inventory.md): (1) inference serving + gateway, (2)
self-hostable inference distribution, (3) tenancy/billing/metering as
commerce machinery, (4) provisioning + CI/CD as delivery machinery.)

**Keep — tenancy & accounts (pillar 3):** `bob2/internal/auth` and the
tenants/users/invites/members/credentials store; the site's
signup/login/forgot-password/admin routes with their Postgres-backed session
plumbing. Real, but commerce machinery in service of the product, not the
product.

**Keep but shrink — billing (pillar 3):** Stripe is wired, but only for
one-time manually-reviewed pilot orders. Subscription mode, webhooks, and
usage metering do not exist. Real work, not a starting-from-zero — still
needed for deliverable 2, still secondary to getting inference serving right.

**Keep — the deployment engine (pillar 4):** `deploy/gcp/` (Terraform + k3s
bootstrap), the connector-worker-pool reconciler pattern in
`bob2/internal/connectorruntime` (~1.8k lines reconciling Deployments/Services
from control-plane rows — generalizes well beyond connectors), and the
hosted-app catalog (OCI Helm chart → hosted instance). This is precisely the
provisioning shape a per-tenant GPU-slot instance (deliverable 2) or a hosted
free-tier inference pod (deliverable 1) needs — the prior pass filed this
under generic "hosting infrastructure"; it is now recognized as a template for
GPU-capacity provisioning specifically, not just any hosted service.

**Keep — pipelines (pillar 4):** the app deploy workflow (Next.js/Go → k3s
over IAP) and the website deploy workflow. Note: the website pipeline builds
from the OSS submodule's `website/` — the private repo owns the *pipeline*,
not the site content.

**Keep as pattern (pillar 1/4, reassessed):** `vald-operator/` — a well-tested
operator skeleton (CRD + CEL + phases + envtest + kind e2e). Originally
surveyed only as "worth mining if per-tenant vector clusters are needed";
under the corrected definition it reads instead as the closest available
reference for a future GPU-slot-lease CRD (deliverable 2's per-tenant
isolation problem is structurally the same shape: a CRD-backed reconciler
handing a scarce resource to one tenant at a time). Worth more than the first
pass gave it credit for — see `ee-inventory.md` undecided #2.

**Wipe:** the pricing/pilot routes (post-pivot dead, already redirecting), the
pre-`bob2` graveyards, `runner/` (direction doc explicitly rejects the
simulation era), connector/hook product surfaces, business-genre docs and the
superseded doc range. `rnd/` is already untracked local material — nothing to
strip.

**Undecided:** `blueprints/` + `packages/{ui,blueprint-renderer}` — these are a
*product*, not hosting infrastructure. They belong wherever that product lives,
if it lives.

## modeld — the important nuance

modeld is **not private code**. It lives in the OSS runtime submodule
(`runtime/modeld`, `runtime/cmd/modeld` — 114 Go files, cgo bindings to
llama.cpp/OpenVINO, with its own per-platform release/checksum pipeline). Two
paths if it is to serve demo/free-tier instances:

1. **Own only the orchestration** — keep depending on the OSS runtime and build
   the layer that runs modeld instances for tenants. Cheap, no new ownership
   questions.
2. **Fork it private** — contradicts the OSS-is-frozen stance recorded in the
   archive's own direction doc. Flagged, not decided.

Either way, modeld today is a *single-owner, one-active-model* daemon (device
leasing, sessions, KV residency). Multi-tenant serving means new routing,
queuing, and quota code **on top of** it — weeks, not days. Whether that new
code should be built on modeld at all, versus serving deliverables 1/2 with
vLLM instead and reserving modeld for deliverable 3, is now a decided
engineering question, not an open one — see
[inference-stack-decision.md](inference-stack-decision.md).

## Gaps, ranked against the three deliverables

The prior pass ranked these as generic SaaS-readiness gaps. Re-ranked against
what actually blocks which deliverable — inference serving substance now
outranks commerce/ops machinery that was previously listed first:

1. **The inference-stack choice itself (a decision, effort-defining):** vLLM,
   modeld, or both, and for which deliverable — settled in
   [inference-stack-decision.md](inference-stack-decision.md). Everything
   below sizes differently depending on this answer.
2. **Multi-tenant inference serving + GPU capacity/isolation (weeks,
   MOVED UP):** routing/queuing/quota across instances, and scheduling/
   bin-packing GPU capacity per tenant. This is the actual product work for
   deliverables 1 and 2 — no part of the surveyed repo does this today; the
   per-tenant provisioning pattern (`connectorruntime`, `vald-operator`) is a
   template, not a solution.
3. **Self-hostable packaging of the inference stack (days–weeks, NEW —
   deliverable 3 was previously an afterthought):** modeld already has a
   working per-OS release/S3 pipeline (Linux verified end-to-end; darwin/
   windows unverified) — the packaging problem for deliverable 3 is closer to
   "finish what exists" than "build from zero," conditional on the stack
   decision above.
4. **Billing (weeks, MOVED DOWN — commerce machinery, not the product):**
   subscription lifecycle, webhooks, usage metering, plan enforcement.
5. **Observability (days–weeks, unchanged rank, still real):** no Prometheus/
   Grafana/Loki in the manifests today; the archive's own screening doc
   already flags this.
6. **Abuse controls for a free tier (days–weeks, unchanged rank):** rate
   limits, bot protection, plan enforcement — the one piece with a live
   asset already (`site/middleware/rateLimit.ts`).
7. **Direction-doc text (RESOLVED, no longer a gap):** the archive's current
   `docs/DIRECTION.md` still states "no hosting full LLMs, no token resale, no
   self-hostable version" in writing. That conflict is now resolved at the
   decision level — WORK.md's "What the private repo IS" explicitly supersedes
   it. The archive's own doc text is simply stale until that repo is next
   touched; nothing here is blocked on it.

## Verdict

The kept core (tenancy, the chart-to-hosted-instance engine, two functioning
CI/CD pipelines behind Terraform-provisioned infrastructure) is real supporting
machinery for all three deliverables, and it shortens the commerce/delivery
half of the work substantially. It does not shorten the *inference* half:
serving models to many tenants, and packaging the same stack for self-hosting,
are the actual product work, and both are new regardless of what this repo
already contains. That work now has a decision attached — see
[inference-stack-decision.md](inference-stack-decision.md) — rather than being
an open-ended "weeks" estimate.
