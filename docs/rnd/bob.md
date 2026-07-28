---
title: "Bob: the hosted document & search dashboard"
description: A multi-tenant SaaS dashboard that turned per-tenant connectors, a real embedding model, and a hosted-apps catalog into one workspace a team could sign up for and run.
---

# Bob: the hosted document & search dashboard

Bob was Contenox's hosted, multi-tenant sibling to the local runtime: a workspace a team signed up for, invited members into with owner/admin/member roles, and pointed at its own connected sources. Two cooperating pieces did the work — a dashboard the browser talked to, and a Go backend holding auth, workspace state, and the connector control plane behind it. End users rode a cookie-based session; the dashboard reached the backend over a short-lived signed service token, so the browser itself never touched the backend directly.

Any properly published connector image became an available source type the moment it landed in the registry — no platform code change required. A workspace picked a source, supplied its credentials (encrypted at rest, wrapped per workspace), and Bob reconciled a connector runtime into a worker pool, synced its documents on a schedule, and made them searchable: chunked, embedded with a real local embedding model, and retrievable by meaning rather than keyword — a paraphrased query in one domain reliably found its matching document among several unrelated ones. A hosted-apps catalog let a workspace deploy self-contained OSS software into its own tenant with the same one-form, install, done shape as every other Bob-managed resource, and a chat workbench let a team ask questions over everything it had connected.


![Bob's workspace dashboard: the onboarding journey from configuring Beam to syncing the first source](/lab-bob-dashboard.png)

Sources were registry-discovered: any properly published connector image showed up as an available type, ready to configure.

![Bob's Sources page listing available connector types — a dummy commerce source and an S3 bucket source](/lab-bob-connectors.png)

Search was a one-time per-workspace setup that picked an embedding model and built the index.

![Bob's search setup: choosing an embedding model before the workspace index is built](/lab-bob-search.png)

The chat workbench sat on top of everything a workspace had connected.

![Bob's Beam workbench: sessions on the left, workspace-context chat on the right, gated behind model and tool setup](/lab-bob-beam.png)

Behind the tenant-facing product, an operator console tracked the workspaces, their worker pools, and the hosted-app catalog.

![Bob's operator console listing the workspaces on the deployment with their slugs and identifiers](/lab-admin-bob-tenants2.png)

## What it proved

- **A single small Go service can carry a real multi-tenant SaaS** — auth, invites, roles, and workspace-scoped everything — on one Postgres, with no framework beyond what the runtime's own API layer already gave it.
- **A connector is just a labeled container image.** Publishing a new source type was a registry push, not a platform release — the catalog grew without touching Bob's own code.
- **Semantic search backed by a real embedding model held up under an honest test:** paraphrased queries across unrelated domains still found the right document by meaning, not word overlap.
- **A hosted-apps catalog proved arbitrary OSS software can become a one-click, per-tenant feature**, provisioned the exact same way as everything else on the platform.

## Where this lives now

The semantic-search lesson lives on directly: `contenox index` plus `workspace_search` is the same real-embedding, ranked-by-meaning retrieval, now run locally over your own codebase instead of a hosted tenant's documents.
