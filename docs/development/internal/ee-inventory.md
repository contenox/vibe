# EE monorepo — granular keep/kill inventory, 2026-07-28

Companion to [ee-hosting-foundation.md](ee-hosting-foundation.md) (read that
first; this document does not repeat its findings). Strictly read-only pass
over the private archive — nothing modified, nothing deployed, no containers
touched. Method: walk top-level dirs, one level deeper where a dir is mixed;
for each entry, grep for cross-references inside the archive and note rough
size. Bias per the maintainer: **when in doubt, keep.** Kill confidence below
answers one question only — "safe to delete today for the inference-product
direction" — not "objectively bad code."

**Pillars (revised 2026-07-28 against the corrected private-repo definition —
see WORK.md, "What the private repo IS": that repo is the LLM inference
product, three deliverables on one stack — hosted free tier, paid GPU-slot
tier, self-hostable edition of the same stack):**

1. **Inference serving + gateway** — the actual product surface for
   deliverables 1 and 2.
2. **Self-hostable inference distribution** — deliverable 3: the same stack,
   packaged to run on one person's hardware.
3. **Tenancy/billing/metering** — commerce machinery in service of the
   product, not the product.
4. **Provisioning + CI/CD** — delivery machinery: how any of the above gets
   built, deployed, and updated.

`—` = maps to neither; usually the parallel "vibecoding V1"/blueprint product
line, or the dead search/simulation eras. `docs/DIRECTION.md` (in the archive)
still states the old "no hosting, no self-host, no token resale" direction in
writing — that conflict is now RESOLVED at the decision level (WORK.md
explicitly supersedes it); the archive's own doc text is simply stale until
someone next touches that repo. No open decision blocks the re-tagging below.

This pass changes two structural things beyond re-tagging: pillar (1) is
mostly *empty* in this repo today (a gap, not an oversight — see the ranked
gap list in `ee-hosting-foundation.md` and the engineering decision in
[inference-stack-decision.md](inference-stack-decision.md)), and pillar (2)
barely exists as private-repo work at all — the real deliverable-3 asset is
modeld itself, which lives in the OSS runtime submodule, not here.

## Root

| Path | What it is | Size | Pillar | Kill confidence | Reusability | Dependencies / blockers |
|---|---|---|---|---|---|---|
| `compose.yaml`, `Dockerfile`, `Makefile`, `package.json`/`package-lock.json`, `go.work`/`go.work.sum`, `lerna.json`, `.yarnrc.yml`/`yarn.lock`, `.gitignore`/`.gitmodules`/`.dockerignore`, `README.md`, `CONTRIBUTING.md`, `CLAUDE.md` | Root tooling/config: dev-stack compose, monorepo build glue, agent rules | ~15 files | 4 | 0% | high — this is the dev-stack definition the survey ran | `go.work` wires `blueprints`, `bob2`, `bob2/tools/openapi-gen`, `connectors/s3`, `runner`, `runtime` as local modules |
| `tmp.md` | Scratch file: a Russian-language video-transcript dump, unrelated to any product surface | 37KB / ~900 lines | — | 100% | none | zero references anywhere in the repo (grepped) |
| `hooks/README.md` | Guardrail note: hook/connector submodules were removed, don't re-add them here | 1 file | 4 | 20% | low | purely informational; near-zero cost to keep as a guardrail |
| `.docsite/` | Generated static HTML export of `docs/` + `business/` (via `scripts/serve-business.sh`) | gitignored, not tracked | — | n/a | n/a | build output, not source; nothing to kill in git |
| `.claude/` | Archive's own Claude Code local settings | 2 files | — | n/a | n/a | tooling config, not product code |
| `.github/workflows/ci.yml` | PR/push compile-smoke for bob2 (incl. cgo llama.cpp embedder), site, packages | 1 file | 4 | 0% | high | gates on `bob2/**`, `site/**`, `packages/**`, `runtime` submodule bump |
| `.github/workflows/deploy.yml` | Builds + deploys site/bob into the existing k3s cluster over IAP | 1 file | 4 | 0% | high — concrete, documented, chained pipeline | needs `infra-apply` to have run; shares `gcp-cluster-mutation` concurrency group |
| `.github/workflows/deploy-website.yml` | Builds contenox.com **from the OSS `runtime/website` submodule** and deploys it; owns the routing cutover switch | 1 file | 4 | 0% | high | the private repo owns this pipeline, not the site content — matches prior survey note |
| `.github/workflows/mirror-images.yml` | Weekly mirror of third-party images into own Artifact Registry (avoids Docker Hub rate limits) | 1 file | 4 | 0% | high | upstream dependency for `deploy.yml`'s pre-flight check |
| `.github/workflows/connectors.yml` | Builds/pushes one image per `connectors/*/Dockerfile` | 1 file | 4 | 0% | high | scans `connectors/*/Dockerfile` by convention; today only `connectors/s3` |
| `.github/workflows/infra-apply.yml`, `infra-plan.yml`, `infra-unlock-state.yml` | Terraform lifecycle: apply+smoke, PR plan, force-unlock | 3 files | 4 | 0% | high | the GCP Terraform provisioning engine's CI half |

## apps/

| Path | What it is | Size | Pillar | Kill confidence | Reusability | Dependencies / blockers |
|---|---|---|---|---|---|---|
| `apps/minio` | Helm chart + `bob.app.json` catalog manifest for MinIO as a hosted app | 9 files | 4 | 0% | high — literal worked example of the chart-to-hosted-instance shape, the same shape a hosted vLLM instance would need | referenced by the app-catalog machinery in `bob2/internal/appcatalog`; used by `compose.yaml`'s `minio`/`minio-init` services locally |

## blueprints/ (Go module, wired into `go.work`)

| Path | What it is | Size | Pillar | Kill confidence | Reusability | Dependencies / blockers |
|---|---|---|---|---|---|---|
| `blueprints/` (whole module: `cmd`, `expr`, `fixtures`, `generate`, `jsonschema`, `merge`, `model`, `schema`, `synthesis`, `ts`, `tsgen`, `validate`) | Blueprint spec/validator/renderer/generator — the V1 build-plan's core deliverable (docs/35) | 56 Go files / ~9.2K LOC | — | 5% | low for the inference product; this *is* the other product's engine | zero cross-module importers today (not yet wired into bob2 despite `go.work`); `blueprints/cmd` is an empty dir; not in any CI workflow or Makefile target — active WIP (commits dated 2026-07-22), not dead legacy. Do not conflate with the search-era zero-importer packages below. |

## bob2/ — top level

| Path | What it is | Size | Pillar | Kill confidence | Reusability | Dependencies / blockers |
|---|---|---|---|---|---|---|
| `bob2/cmd/bob` | Main server binary entrypoint | 1 file | 3/4 | 0% | high | imports `internal/server`, which wires everything else — including wherever pillar-1 gateway routes eventually land |
| `bob2/cmd/connector` | Generic connector-runtime binary (the per-connector container image entrypoint) | 1 file | 4 | 0% | high | pairs with `internal/connectorruntime` |
| `bob2/cmd/dummy-connector-service` | Demo connector ("Dummy Commerce" in the harvested screenshots) proving the connector shape end to end | 1 file | 4 | 10% | med — nice demo/example, not needed in prod | built via `bob2/Dockerfile.dummy-connector` |
| `bob2/cmd/rotate-encryption-key` (source) | Encryption-key rotation utility, source form | 1 file + `envelope/` | 3 | 0% | med | pairs with `internal/vault` |
| **`bob2/rotate-encryption-key`** (top-level, no source ext) | **Compiled Go binary, checked into git** — an ELF executable, 15MB, unstripped, with debug info | 16MB, 1 file | — | **100%** | none | superseded by `bob2/cmd/rotate-encryption-key/main.go`, which rebuilds it fresh; confirmed git-tracked (not gitignored) |
| `bob2/bin/openapi-gen` | Local build output of `bob2/tools/openapi-gen` | 1 file | — | n/a | n/a | gitignored (`bob2/.gitignore: bin/`), not tracked — nothing to kill in git |
| `bob2/apitests/` | Python/pytest black-box API test suite against the running bob server (18 test files, incl. `test_simulations.py`, `test_runners.py`, `test_signup_and_auth.py`, `test_fleet.py`) | 20 files (+ `.venv`, `__pycache__`) | 3/4 | 0% overall | high — exercises the real auth/tenancy/connector/fleet surface | `.venv` and `__pycache__` are local, uncommitted (verify gitignore before any cleanup); `test_simulations.py`/`test_runners.py` (522 LOC) test the dead-direction slice specifically — see Quick kills |
| `bob2/tools/openapi-gen` | Standalone Go tool: generates OpenAPI spec from annotated handlers | 1 module | — | 0% | high — generic, reused by `internal/apiframework`'s consumers | own `go.mod`, wired into `go.work` |

## bob2/internal/* (one level deeper, per the brief)

| Path | What it is | Size | Pillar | Kill confidence | Reusability | Dependencies / blockers |
|---|---|---|---|---|---|---|
| `apiframework` | Handler-annotation → OpenAPI-spec generation core | 9 files / 1.0K LOC | — | 0% | high — generic Go-handler-to-spec machinery, usable for any control-plane API including a future gateway | 32 in-repo importers, the most widely depended-on package after `auth`/`store` |
| `appcatalog` | Hosted-app catalog: OCI Helm chart → hosted instance | 6 / 1.1K | 1/4 | 0% | high — the chart-to-hosted-instance mechanism is a direct candidate for deploying hosted vLLM instances per tenant, not just generic apps | 11 importers; pairs with `apps/minio`, `apprelease`, `appsource` |
| `apprelease` | App release lifecycle for the catalog | 4 / 1.0K | 1/4 | 5% | high | 1 importer (routed through `appcatalog`'s service surface) |
| `appsource` | App source/registry resolution for the catalog | 2 / 275 | 1/4 | 10% | med | 1 importer |
| `auth` | Tenants/users/invites/members/credentials/sessions | 15 / 1.9K | 3 | 0% | high — the core commerce-machinery asset (pillar 3) | 51 importers, load-bearing across the whole server |
| `beamservice` | Tenant-scoped Beam chat sessions/messages, wiring the OSS runtime's own `chatservice`/`enginesvc`/`sessionservice`/`taskengine`/`runtimestate` per tenant | 2 / 876 | 1 | 5% | high, reassessed — this already wires the OSS harness engine per tenant; it is the closest thing in the repo to a seed of the multi-tenant hosted-session serving path deliverable 1 needs. Previously undervalued as a generic "d"-pillar demo front end under the old SaaS framing | 5 importers |
| `blueprints` (internal, vendored copy of `merge`/`model` from the top-level `blueprints/` module, kept in sync via `vendorsync_test.go`) | V1 blueprint-storage integration scaffolding | 8 / 1.0K | — | 15% | low for the inference product | active WIP (2026-07-22), 0 non-test importers — not yet wired, not dead |
| `blueprintservice` | V1 blueprint service layer | 3 / 760 | — | 15% | low for the inference product | active WIP, 0 non-test importers |
| `chunker` | Text chunking for the (dead) search/RAG pipeline | 6 / 847 | — | 55% | low | 3 importers; part of the search-era retrieval chain, isolable but wired |
| `chunkstore` | Chunk storage for search/RAG | 9 / 1.1K | — | 55% | low | 8 importers; same chain as `chunker` |
| `config` | App config loader | 1 / 233 | — | 0% | high — generic | 7 importers |
| `connector` | Connector abstraction/model | 9 / 939 | 4 | 0% | high | 6 importers; core to pillar 4 |
| `connectorregistry` | Registry of available connector images | 2 / 1.16K | 4 | 0% | high | 3 importers |
| **`connectorruntime`** | **The reconciler pattern**: reconciles k8s Deployments/Services from control-plane rows for per-tenant connector worker pools | 6 / 1.78K | 1/4 | 0% | **high — generalizes well beyond connectors; this is the leading template for per-tenant GPU-slot provisioning (deliverable 2), not just connector worker pools, per the corrected definition** | 4 importers; references `connectors/s3` in its own kubernetes_test.go as a worked fixture |
| `embed` | In-process llama.cpp embedder (cgo) | 4 / 355 | 1 | 5% | **high — ANSWERED (was undecided #4): keep. This is on-product cgo llama.cpp integration, direct evidence the inference-embedding path already works in this codebase, independent of whether the chat-completion engine ends up being vLLM or modeld** | 1 importer; only non-pure-Go package (cgo), called out separately in `ci.yml` for its own compile step |
| `entityservice` | V1 entity/CRUD-synthesis DSL (docs/36 data model) | 6 / 2.0K | — | 15% | low for the inference product | active WIP (2026-07-22), 0 non-test importers, largest unwired package in the tree |
| `eventdispatch` | Domain event dispatch | 2 / 334 | 4 | 10% | med | 8 importers |
| `eventsource` | Event source registration | 1 / 280 | 4 | 10% | med | 9 importers |
| `eventstore` | Event persistence | 7 / 989 | 4 | 5% | high | 17 importers |
| `files` | Tenant file storage (VFS-adjacent) | 2 / 383 | 3/4 | 5% | high | 8 importers |
| `functionexec` | goja function execution | 3 / 293 | — | 20% | med | 1 importer |
| `functionservice` | Function service wrapper | 1 / 84 | — | 30% | low | 3 importers |
| `functionstore` | Function definitions storage | 5 / 589 | — | 20% | med | 4 importers |
| `indexbridge` | Search-index bridge | 2 / 453 | — | 55% | low | 1 importer |
| `indexconfig` | Search-index config | 5 / 393 | — | 50% | low | 8 importers |
| `indexconsumer` | Search-index event consumer | 4 / 501 | — | 55% | low | 1 importer |
| `indexservice` | Search-index service | 5 / 900 | — | 50% | low | 8 importers |
| `ingest` | Document ingestion for search | 2 / 508 | — | 50% | low | 4 importers |
| `ragharness` | RAG evaluation harness | 3 / 392 | — | 70% | low | 0 non-test importers — most isolated package in the search chain |
| `searchmodels` | Search domain models | 2 / 202 | — | 45% | low | 5 importers |
| `server` | HTTP server/router — the biggest package in the repo | 44 / 12.2K | 3/4 | 0% overall | high — the live routing surface, and where pillar-1 gateway endpoints would eventually be added | 1 importer (`cmd/bob`, expected — it's the top of the graph). **Mixed**: contains `scenarios.go`+`runners.go`+`runner_api.go`+`runners_test.go` (~1.3K LOC) implementing the rejected simulation/runner-registry feature — see Quick kills |
| `sourcesync` | Interval sync scheduler for search sources | 1 / 50 | — | 70% | low | 2 importers; tiny, cleanly search-era |
| `store` | General Postgres repository layer for the whole app (tenants, users, auth, connectors, app_stack, credentials, events, invites...) | 39 / 8.2K | 3/4 | 0% overall | high — the generic data-access layer | 70 importers, second only to `auth`. **Mixed**: `sim_moves.go`+`sim_run_parties.go`+`sim_runs.go`+`sim_scenarios.go`+`runners.go` (~1.2K LOC) are simulation/runner-registry tables — see Quick kills |
| `vault` | Tenant secrets/encryption | 2 / 533 | 3 | 0% | high | 4 importers |
| `vectorstore` | Tenant-scoped SQLite vector store (current, not Vald) | 2 / 378 | — | 35% | low-med | 5 importers; **reclassified**: previously tagged pillar "d" (modeld-adjacent) by association with the search-era `embed` package's neighborhood; under the corrected definition this is a per-tenant RAG storage pattern, not inference serving — it does not become on-product just because it sits near `embed`. Kill confidence raised accordingly (was 20%). Still isolable, still low urgency; confirms `vald-operator` is not the live vector backend |
| `version` | Build version stamping | 1 / 21 | — | 10% | low | 5 importers, trivial either way |
| `vfsservice` | Virtual filesystem service | 5 / 1.9K | 3/4 | 0% | high — reused by the V1 blueprint-storage plan too | 18 importers |
| `vfsstore` | VFS storage backend | 9 / 1.2K | 3/4 | 0% | high | 16 importers |
| `workercluster` | Worker-cluster registration for the reconciler pattern | 1 / 174 | 1/4 | 5% | high — same GPU-slot-provisioning relevance as `connectorruntime` | 2 importers |

## business/

| Path | What it is | Size | Pillar | Kill confidence | Reusability | Dependencies / blockers |
|---|---|---|---|---|---|---|
| `business/00-index.md`, `business/TEMPLATE.md` | Index + doc template, governed by `scripts/docs-lint.sh` | 2 files | — | 10% | low | lint script checks Status/Updated headers and index membership |
| `business/archive/` | 3 dead plan docs (simulation deck, sovereign-AI legacy plan, DORA plan) | 3 files | — | 90% | none | explicitly named "archive"; superseded per docs/00-index era markers |
| `business/brand/` | Logo assets | 7 files | 4 | 20% | high, but trivially regenerable | populated by `scripts/sync-brand.sh` from the OSS `runtime/website/public` — not an original source, safe to wipe and re-sync anytime |
| `business/pitch/` | Pitch deck (html+md) | 2 files | — | 60% | low | pre-pivot pitch content; not the inference-product narrative — see undecided #6 |
| `business/plan/` | 8-file business plan (market, model, GTM, financials, risks) | 8 files | — | 60% | low | pre-pivot business plan; direction has moved three times since (search→simulation→V1/hosting→inference product) |

## connectors/

| Path | What it is | Size | Pillar | Kill confidence | Reusability | Dependencies / blockers |
|---|---|---|---|---|---|---|
| `connectors/README.md` | Convention doc: connector dirs need `Dockerfile`+`manifest.json`+`cmd/<name>-connector-service` | 1 file | 4 | 0% | high | describes the CI discovery convention `connectors.yml` implements |
| `connectors/s3` | S3 connector image (real, deployable) | 1 module / 359 LOC | 4 | 0% | high — worked example of the connector image contract | built by `connectors.yml`; referenced as a fixture in `connectorruntime`'s own test; reuses `bob2/go.mod` in its Dockerfile (noted as "interim" in the README itself) |

## deploy/

| Path | What it is | Size | Pillar | Kill confidence | Reusability | Dependencies / blockers |
|---|---|---|---|---|---|---|
| `deploy/gcp` | Terraform (VPC, VMs, k3s bootstrap on a gateway VM) + rendered k8s manifests (`bob.yaml`, `site.yaml`, `website.yaml`, `website-ingress.yaml`, `connector-gateway.yaml`, `cluster-issuer.yaml`, `smoke-test.yaml`) + bootstrap/deploy scripts | 15 files | 4 | 0% | **high — the mature provisioning engine**, and the substrate a GPU-node-pool addition would extend rather than replace | needs a real GCP project/IAM/DNS to run; consumed by `infra-apply.yml`, `infra-plan.yml`, `deploy.yml`, `deploy-website.yml` |
| `deploy/connector-worker-pool` | Sample kubeconfig-generation script + gateway/worker-pool manifests for a tenant worker cluster | 4 files | 1/4 | 5% | high — same GPU-slot relevance as `connectorruntime`/`workercluster` | pairs with `bob2/internal/connectorruntime` + `workercluster` |
| `deploy/local` | `postgres-init.sql` for the compose stack | 1 file | 4 | 0% | high | consumed by `compose.yaml`'s `postgres` service |

## docs/ (grouped by era — see `docs/00-index.md` in the archive for the authoritative grouping; not repeated file-by-file here)

| Path | What it is | Size | Pillar | Kill confidence | Reusability | Dependencies / blockers |
|---|---|---|---|---|---|---|
| `docs/DIRECTION.md` + `docs/00-index.md` | The one file allowed to state product direction, + its index | 2 files | — | 0% | n/a — this is governance, not code | **RESOLVED (was an open conflict in the prior pass):** the doc still states "no self-hostable version, no hosting full LLMs, no token resale" in writing, but that conflict is settled at the decision level — WORK.md's "What the private repo IS" explicitly supersedes it. The archive's own text is stale until that repo is next touched; nothing here blocks on it |
| Current V1 docs (34–39: api-spec-boundary, v1-build-plan, data-model, authoring-format, events-and-triggers, authn-authz) | Active design docs for the blueprint/vibecoding product | 6 files | — | 5% | low for the inference product | current per the archive's own index; unrelated to pillars 1–4 but not stale |
| Explorations/parked (41: ts-surface-go-engine) | Parked design thread | 1 file | — | 20% | low | explicitly marked parked, not direction |
| Substrate/infra records (03,04,05,09,10,11,12,14,15, vfs/vectorstore design + migration docs, rag-baseline, indexing-bridge-implementation, operator-landscape, operator-crd-draft, cross-repo-inventory) | Descriptive records of machinery that still exists and is reused (auth, reconciler, VFS, vector store) | ~17 files | 3/4 | 10% | high as documentation of the kept core | still-accurate descriptions of packages marked keep above; `rag-baseline`/`operator-*` describe the now-superseded Vald-era retrieval baseline specifically |
| Search/Dashboard era (01,02,06,07,08,13,16–25, build-plan, ingested-doc-spec) | Design records of the rejected workplace-search product | ~18 files | — | 85% | low — historical record only | every doc self-stamped as dead per docs/00-index; describes the same code flagged medium-kill above (`chunker`/`indexservice`/etc.) |
| Simulation era (26–33, guides/e2e-walkthrough, research/*) | Design records + mined external research for the rejected simulation/digital-twin product | ~16 files | — | 85% (design docs) / 30% (research/*) | low for design docs; the `research/` mined facts (DORA, tabletop, wargaming sources) stand on their own if any future product wants that domain knowledge | matches the `runner/` module and the simulation slice in `bob2`/`site` flagged below |

## packages/

| Path | What it is | Size | Pillar | Kill confidence | Reusability | Dependencies / blockers |
|---|---|---|---|---|---|---|
| `packages/ui` (`@contenox/ui`) | Private component library (distinct from the OSS `runtime/packages/ui`) | 66 files / 4.5K LOC | — | 10% | high as an asset, but currently a V1-product dependency | consumed mainly by `packages/blueprint-renderer`'s block library; `site` only pulls its CSS (2 references, no component imports) — not a `site/package.json` dependency at all, resolved purely via `next.config.ts`/`tsconfig.json` path aliases into `../packages/*/dist` |
| `packages/blueprint-renderer` (`@contenox/blueprint-renderer`) | Renders the closed blueprint/pages DSL to React | 37 files / 2.1K LOC | — | 5% | low for the inference product, but **live today**: `site/app/page.tsx` + `site/app/landing-blueprint.tsx` render the current homepage through it | breaks the live site homepage if removed without replacing the landing page; compose.yaml doesn't mount `./packages` into the `site` container (known bug, see prior survey) |

## rnd/

| Path | What it is | Size | Pillar | Kill confidence | Reusability | Dependencies / blockers |
|---|---|---|---|---|---|---|
| `rnd/` (amplication, bolt.diy, direktiv, Eve, lowdefy, mining, parse-server, puck, sandstorm, windmill + 2 transcript .md files) | Cloned OSS repos + scratch notes mined for R&D ideas | 12 entries | — | n/a | n/a — **fully gitignored, untracked local material**; nothing in git to strip, confirmed via `git check-ignore` | not part of the archive's tracked history at all |

## runner/ (Go module — the simulation engine)

| Path | What it is | Size | Pillar | Kill confidence | Reusability | Dependencies / blockers |
|---|---|---|---|---|---|---|
| `runner/cmd/runnerd`, `runner/internal/{engine,evidence,grounding,panel,state,supervisor}`, `runner/internal/config` | The dial-home simulation-persona engine (panel/scenario runner) | 24 Go files / 6.5K LOC | — | 75% | low for the inference product; **the dial-home/enrollment *pattern* is cited as prior art in the archive's own direction doc** for "hosting the results" on customer compute | zero cross-module Go importers (separate `go.mod`); DIRECTION.md explicitly rejects the simulation era as product — this is the clearest large "provably unneeded" module, but the pattern (not the code) is worth re-reading before deleting |
| `runner/cmd/kvforkbench`, `runner/kvforkbench` | KV-fork fan-out benchmark tooling | small | — | 80% | low | referenced only by docs/30 (simulation era) |
| `runner/_smoke`, `runner/README.md` | Smoke fixtures/docs for the above | small | — | 80% | low | same fate as the module |

## scripts/

| Path | What it is | Size | Pillar | Kill confidence | Reusability | Dependencies / blockers |
|---|---|---|---|---|---|---|
| `scripts/build-site-bundles.sh` | Builds `packages/ui` + `packages/blueprint-renderer` dist bundles that `site` consumes via path alias | 1 file | 4 | 0% | high | consumed by `ci.yml`'s site job and `deploy.yml`'s Docker build; build order matters (ui before blueprint-renderer) |
| `scripts/docs-lint.sh` | Lints `business/**/*.md` headers/index/links | 1 file | — | 20% | low | scoped to `business/` only |
| `scripts/serve-business.sh` | Renders `business/`+`docs/` to `.docsite/` and serves locally | 1 file | — | 20% | low | feeds the gitignored `.docsite/` output |
| `scripts/sync-brand.sh` | Copies canonical brand assets from the OSS `runtime/website/public` into `site/public/brand` and `business/brand` | 1 file | 4 | 0% | high | single source of truth is the OSS repo; this is a one-way sync, never hand-edit the copies |

## site/ (Next.js app)

| Path | What it is | Size | Pillar | Kill confidence | Reusability | Dependencies / blockers |
|---|---|---|---|---|---|---|
| `site/app/admin` | Operator console (overview/workspaces/worker-pools/hosted-apps tabs per harvested screenshots) + admin login | 12 files | 3/4 | 0% | high | operator-facing surface over `appcatalog`/`workercluster`/`auth` |
| `site/app/api` | Next.js API routes: `admin` (17f/599L), `bob` (56f/1.5K — proxy to bob2), `checkout` (76L, Stripe), `login`/`logout`/`me`/`password`/`signup` (auth), `analytics`, `providers`, `setup` | 82 files / ~2.5K LOC | 1/3/4 | 0% | high | `bob` subtree is the largest — a thin proxy layer to the bob2 server API, and where any gateway/inference route would surface client-side |
| `site/app/bob` | Tenant dashboard: activity, ai, apps, beam, billing, chat, connectors, events, files, invites, members, runners, search, simulations + workspace shell | 14 subdirs / ~8.8K LOC | 1/3/4 (mixed) | mixed | high for most subdirs | **`beam` pairs with `bob2/internal/beamservice` — pillar 1, reassessed up from generic tenancy.** `simulations` (1.56K LOC) and `runners` (420 LOC) implement the rejected simulation/dial-home-registry UI — still nav-wired (`workspace-shell.tsx`, `layout.tsx` reference it), not orphaned in the UI sense, but dead-direction — see Quick kills. `billing` (258L) matches the "pilot checkout only" state from the prior survey (pillar 3) |
| `site/app/login`, `site/app/signup`, `site/app/forgot-password`, `site/app/reset-password`, `site/app/invite/[token]` | Auth flow pages | 8 files | 3 | 0% | high | pillar-3 core, Postgres-backed sessions per prior survey |
| `site/app/pricing`, `site/app/services` | Both are one-line `redirect("/")` stubs | 2 files | — | **95%** | none | confirmed dead: both files are pure redirects, nothing else references them |
| `site/app/legal` | Legal/ToS page | 1 file | 4 | 0% | med | generic site content |
| `site/app/layout.tsx`, `page.tsx`, `landing-blueprint.tsx`, `error.tsx`, `robots.ts`, `sitemap.ts`, `globals.css`, images | Root app shell + the blueprint-DSL-rendered homepage | ~10 files | 4/— | 5% | high | homepage rendering depends on `packages/blueprint-renderer` (see above) — not independently killable without a replacement homepage |
| `site/components/admin` | Admin-console components | 5 files | 3/4 | 0% | high | backs `site/app/admin` |
| `site/components/ui` | Local shadcn-style UI primitives (button/card/badge/etc.) | 25 files | 4 | 0% | high | used throughout `site/app`; largely independent of `packages/ui` |
| `site/components/*.tsx` (navbar, footer, analytics-provider, cookie-notice, theme-provider, status-pill, page-header, page-event-tracker, ErrorBoundary, logo-mark) | Shared site chrome | 10 files | 4 | 0% | high | generic site scaffolding |
| `site/db/migrations` | Postgres migrations for the site's own tables (sessions, checkout orders, etc.) | 4 files | 3 | 0% | high | run via `site/scripts/migrate.mjs` |
| `site/lib/platform/*` | Server-side platform lib: auth, bob-route proxy, checkout, db, email, env, feature-flags, github, invites, notifications, password-reset, analytics | 18 files | 3/4 | 0% | high | the meat of the site's backend-for-frontend layer |
| `site/lib/{download-helpers,error-text,markdown,simulations,utils,validation}.ts` | Misc lib helpers | 6 files | mixed | mixed | `simulations.ts` — 5% (dead direction, has its own test file); rest 0% (generic) | `site/tests/lib/simulations.test.ts` exists too — part of the same isolable slice |
| `site/hooks/*` | Small React hooks (useDebounce, useFetch, useLoadingState, useModalState, useTimeout, use-toast) | 6 files | 4 | 0% | high | generic |
| `site/middleware/rateLimit.ts` | Request rate limiting against Postgres | 1 file | 1 | 0% | high — **reclassified from generic-SaaS "abuse control" to pillar 1: this protects whatever inference gateway sits behind it, which is precisely the free-tier abuse-control gap ranked up in `ee-hosting-foundation.md`** | uses `lib/platform/db` |
| `site/public/*` | Static assets: logos, favicons, product screenshots/videos (beam demo, HITL diagrams, chain-flow diagram) | ~25 files | 4 | 30% | mixed — brand assets high, product screenshots (hitl/chain-flow/aionui) low relevance to hosting | some screenshots are for product surfaces (HITL, chains) that belong to the OSS runtime story, not this repo's role |
| `site/scripts/*.mjs` | Build/dev scripts: docs-search-index build, db migrate, next-dev wrapper, doc-text normalizer (+its test) | 5 files | 4 | 0% | high | `migrate.mjs` runs `site/db/migrations` |
| `site/tests/{guard,lib,platform}` | Vitest suites: no-oss-content guard, landing-blueprint guard, lib/platform unit tests | 10 files | 4 | 0% | high, except `lib/simulations.test.ts` (dead-direction) | `guard/no-oss-content.test.ts` is notable — an active leak-discipline check worth understanding before any repo restructuring |
| `site/STRIPE_SETUP.md` | Stripe pilot-checkout setup doc | 1 file | 3 | 0% | high | matches "billing wired for one-time pilot orders only" from prior survey |

## vald-operator/ (Go module, kubebuilder-style operator)

| Path | What it is | Size | Pillar | Kill confidence | Reusability | Dependencies / blockers |
|---|---|---|---|---|---|---|
| `vald-operator/api/v1alpha1`, `internal/controller`, `internal/phases`, `cmd` | `ValdRelease` CRD + reconciler: CRD+CEL+phases+envtest+kind e2e | 13 Go files / 1.6K LOC | 1/4 (pattern) | 25% | **reassessed up — the CRD+reconciler pattern is the closest available reference for a future GPU-slot-lease CRD (deliverable 2), not just an orphaned vector-DB pattern.** Kill confidence lowered from 55%: near-term reuse is now plausible, not merely hypothetical | confirmed zero references from `compose.yaml`, `deploy/`, or `bob2` — the live vector backend is SQLite (`bob2/internal/vectorstore`), not Vald. Orphaned as a live dependency; live only as a reference implementation |
| `vald-operator/config`, `test/e2e`, `Makefile`, `PROJECT` | Kubebuilder scaffolding around the above | 19 files | 1/4 (pattern) | 25% | med, same reassessment as above | same fate as the controller code |

## runtime/ (git submodule — the OSS repo)

| Path | What it is | Size | Pillar | Kill confidence | Reusability | Dependencies / blockers |
|---|---|---|---|---|---|---|
| `runtime/` | Pinned submodule checkout of `contenox/runtime` (OSS, `main` branch) | n/a | 1/2 | n/a | n/a — not private, out of this inventory's scope | **reassessed**: this is where the actual pillar-1 engine (`beamservice`'s dependencies: chatservice/enginesvc/taskengine/runtimestate) AND the pillar-2 asset (`modeld`, `cmd/modeld`, the per-OS release pipeline) both live. Previously tagged generically "c/d"; it is now the single most important external dependency for both live deliverables | `go.work` resolves `bob2` against it locally; `deploy-website.yml` builds contenox.com from `runtime/website`; `scripts/sync-brand.sh` sources brand assets from `runtime/website/public`. See [inference-stack-decision.md](inference-stack-decision.md) for the modeld/vLLM engineering question this submodule feeds |

## rows not itemized

`site/node_modules`, `node_modules`, `.venv`/`__pycache__` under `bob2/apitests`, `site/.next`, `bob2/bin`, dist/ build outputs under `packages/*` — all gitignored/vendored, ~1.6GB combined, zero decision content. Not itemized.

---

## 1. Quick kills

**Provably dead, zero cost to remove, no caveats:**
- `bob2/rotate-encryption-key` (checked-in 15MB compiled binary, superseded by its own source) — **16MB**.
- `tmp.md` (unrelated scratch transcript) — 37KB.
- `site/app/pricing`, `site/app/services` (both `redirect("/")` stubs) — trivial size, but confirm nothing external links to `/pricing` before pulling (marketing links, old decks) since the redirect currently masks that.

**Total reclaimed from the two provable kills: ~16MB.** Everything else below needs at least a skim before deletion.

**The simulation/runner-registry vertical slice** (rejected direction per `docs/DIRECTION.md`, self-contained — zero external importers found outside this cluster):
- `runner/` module (6.5K LOC, separate `go.mod`) — the actual simulation engine (panel/evidence/grounding/supervisor).
- `bob2/internal/server`'s `scenarios.go`+`runners.go`+`runner_api.go`+`runners_test.go` (~1.3K LOC).
- `bob2/internal/store`'s `sim_moves.go`+`sim_run_parties.go`+`sim_runs.go`+`sim_scenarios.go`+`runners.go` (~1.2K LOC).
- `site/app/bob/simulations` + `site/app/bob/runners` + `site/lib/simulations.ts` (~2K LOC), still nav-wired in `workspace-shell.tsx`.
- `bob2/apitests/test_simulations.py` + `test_runners.py` (522 LOC).
- Docs 26–33 + guides/research (already counted in the docs table).

**~11–12K LOC total, one coherent feature, cleanly traceable top to bottom.** Caveat: it's live and tested today (not orphaned in the compiler sense), so pulling it means a real PR — remove nav entries, drop `sim_*`/`runners` tables via migration, delete the apitests, retire the `runner` submodule reference from `go.work`. Not a five-minute delete, but a well-scoped one.

**The search/RAG era** (dead per docs/00-index, wired but isolable):
- `bob2/internal/{chunker,chunkstore,indexbridge,indexconfig,indexconsumer,indexservice,ingest,ragharness,searchmodels,sourcesync}` — ~6.1K LOC combined.
- `vald-operator/` (1.6K LOC) — confirmed zero live dependency (vector backend is SQLite, not Vald), kept only as an operator-pattern reference — see the reassessment above, which raises the value of that reference without changing the live-dependency verdict.
- Docs era group "Search/Dashboard" (18 files, already counted).

Caveat: `embed` (in-process llama.cpp embedder) and `vectorstore` (SQLite) sit in the same neighborhood but are flagged separately above at different kill-confidence — `embed` is now an answered keep (pillar 1), `vectorstore` is reclassified out of the inference-adjacent halo (pillar —, kill confidence raised).

## 2. Load-bearing core

What must survive, and what breaks without it:

- **`bob2/internal/auth` + `store` + `vfsservice`/`vfsstore`** — pillar 3. Breaks: every login, tenant, invite, session, and file operation; `store` alone has 70 in-repo importers.
- **`bob2/internal/{connector,connectorregistry,connectorruntime,workercluster}` + `appcatalog`/`apprelease`/`appsource`** — pillar 4 (and the direct template for pillar 1/2 GPU-slot provisioning). Breaks: the entire hosted-instance reconciliation loop — this is the single most reusable asset in the repo per the prior survey, and confirmed here by import density and the worked `apps/minio` + `connectors/s3` examples.
- **`deploy/gcp` (Terraform+k3s) + `deploy/connector-worker-pool`** — pillar 4. Breaks: all cluster provisioning; nothing else in the repo can stand up a k3s cluster or a tenant worker pool.
- **`.github/workflows/{ci,deploy,deploy-website,mirror-images,connectors,infra-apply,infra-plan,infra-unlock-state}.yml` + `scripts/build-site-bundles.sh`** — pillar 4. Breaks: both CI/CD pipelines (site+bob deploy, and the separate contenox.com static-site deploy from the OSS submodule); `build-site-bundles.sh` breaks the site build specifically (packages/ui and blueprint-renderer dist bundles it depends on via path alias, with the known compose mount gap noted in the prior survey).
- **`bob2/internal/apiframework`** — substrate, not a named pillar, but 32 importers deep; breaks any handler that expects a generated OpenAPI spec, which likely includes future gateway APIs too.
- **`bob2/internal/vault`** — pillar 3. Breaks: tenant secret/credential storage, which connector-runtime and auth both depend on.
- **`site/app/{admin,api,bob}` (minus the simulations/runners slice) + `site/lib/platform/*` + `site/middleware/rateLimit.ts`** — pillars 3/4, with `rateLimit.ts` now recognized as pillar 1 (the one piece of the free-tier gateway abuse-control gap that already exists).
- **NEW — pillar 1, thin but real: `bob2/internal/{beamservice,embed}`.** This is the entire direct inference-serving asset in the repo today. It is small relative to pillars 3/4, which is exactly why deliverables 1 and 2 need substantial new code regardless of what else is kept — see the ranked gap list in `ee-hosting-foundation.md` and the engineering decision in [inference-stack-decision.md](inference-stack-decision.md).

## 3. Undecideds worth a maintainer minute

1. **~~Does `docs/DIRECTION.md`'s "no self-hostable version / no hosting full LLMs / no token resale" stance get superseded in writing before any deletions happen?~~ ANSWERED:** yes, already superseded — WORK.md's "What the private repo IS" is the settled definition. The only remaining action is mechanical (editing the archive's own `DIRECTION.md` text next time that repo is touched); it blocks nothing today.
2. **RESTATED (was: "is `vald-operator/` worth keeping for per-tenant vector clusters"):** the vector-cluster framing is dropped — per-tenant vector isolation is not part of the inference product. The live question is different: is the `ValdRelease` CRD+reconciler skeleton worth formalizing now as the starting point for a GPU-slot-lease CRD (deliverable 2), or left parked as a reference until GPU-slot provisioning is actually being built? Leaning toward the latter (don't build ahead of need), but flagging the reuse explicitly changes the "safe to delete" answer — see its lowered kill confidence above.
3. **Does the rejected simulation/runner-registry slice get ripped out now, or left alone until something else needs the `sim_*` tables' migration slot?** Unaffected by the corrected definition — it's dead-direction either way. It's tested and shipping today; removing it is a real (if well-scoped) PR, not a no-op.
4. **~~Is `bob2/internal/embed` worth carrying forward as day-one modeld-adjacent plumbing for pillar d?~~ ANSWERED YES:** keep. It is on-product (pillar 1) regardless of the vLLM-vs-modeld engineering decision — see [inference-stack-decision.md](inference-stack-decision.md).
5. **Do `packages/ui` + `packages/blueprint-renderer` stay wired into the live site homepage, or does a non-blueprint-DSL homepage get built?** Orthogonal to the inference-product definition — this is entirely a V1/blueprint-product question (both packages are tagged `—`). Decide independently within that product track; nothing here changes it.
6. **RESTATED (was: "refresh `business/plan`+`business/pitch` for the hosting-foundation narrative"):** the narrative to refresh against, if refreshed at all, is now the three-deliverable inference-product framing (free/paid/self-hostable, capacity-not-capability) — not a generic "hosting foundation" pitch. Still a low-priority, low-cost-to-defer item; restating only to keep the target framing correct if anyone picks it up.
