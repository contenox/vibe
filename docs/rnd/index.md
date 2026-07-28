---
title: "Lab"
description: The contenox Lab — research built, shipped, and run for real. Beam web UI, modeld, the VS Code extension, the HTTP API layer, the UI component library, Bob, runnerd, vald-operator, and blueprints.
---

# Lab

Research, built for real. Every line here was shipped and run — each one feeds the runtime, the envelope model, and the harness you use today.

- **[Beam: the web UI](/docs/rnd/beam-web/)** — a full React admin + chat SPA served straight out of the `contenox` binary, with a diff-backed approval gate and a live workspace file tree.
- **[modeld](/docs/rnd/modeld/)** — a cross-backend local inference daemon with a live-hardware capacity planner, lease-based multi-client ownership, and llama.cpp/OpenVINO backends behind one gRPC contract.
- **[VS Code extension](/docs/rnd/vscode-extension/)** — an editor-native `@contenox` chat participant, autocomplete, and gated tool execution, distributed through the Marketplace and remote dev hosts.
- **[The HTTP API layer](/docs/rnd/api-layer/)** — `apiframework` plus a from-scratch OpenAPI generator that read typed request/response contracts straight out of route annotations and enforced them in CI.
- **[The UI component library](/docs/rnd/ui-library/)** — `@contenox/ui`, a versioned, Storybook-catalogued React design system spanning chat, terminal, and visual workflow components.
- **[Bob: the hosted document & search dashboard](/docs/rnd/bob/)** — a multi-tenant SaaS dashboard with pluggable source connectors, real-embedding semantic search, and a one-click hosted-apps catalog.
- **[runnerd: the self-hosted simulation runner](/docs/rnd/runner/)** — an outbound-only agent that ran branching, human-and-AI tabletop exercises and shipped an auditable, redaction-safe evidence bundle.
- **[vald-operator](/docs/rnd/vald-operator/)** — a Kubernetes operator provisioning one vector-search cluster per tenant, with its unsafe fields made immutable at the API boundary.
- **[blueprints](/docs/rnd/blueprints/)** — a schema-validated page format plus a bounded AI draft-and-repair loop that turned a plain-English ask into a working, edit-preserving app.
