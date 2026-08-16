---
title: "Lab"
description: The contenox Lab — research built, shipped, and run for real. The event plane, Beam web UI, Beam Desktop, modeld, the editor agent, hooks, the HTTP API layer, the UI component library, Bob, runnerd, vald-operator, the early Telegram/GitHub MVP, blueprints, and what measuring the hand-written codebase against its AI-assisted successor showed.
---

# Lab

Research, built for real. Every line here was shipped and run — each one feeds the runtime, the envelope model, and the harness you use today.

- **[The event plane](/docs/rnd/event-plane/)** — the consumer side of a durable event log extracted into the importable `libevents` — cursors, primary-key firing claims, stale-claim takeover — with operator-authored triggers firing task chains on durable events under human-in-the-loop envelopes.
- **[Event-driven chains: three stories](/docs/rnd/event-driven-chains/)** — the trigger tier in use: a phone buzz when a mission asks for a human, a completion summary written when a mission ends, and the firing record that makes both inspectable.
- **[Beam: the web UI](/docs/rnd/beam-web/)** — a full React admin + chat SPA served straight out of the `contenox` binary, with a diff-backed approval gate and a live workspace file tree.
- **[Beam Desktop: the client as a peer](/docs/rnd/beam-desktop/)** — an Electron shell that deleted HTTP from the loading path, spawned the runtime as a child process, and carried both ACP and a private editor method family over one NDJSON pipe.
- **[modeld](/docs/rnd/modeld/)** — a cross-backend local inference daemon with a live-hardware capacity planner, lease-based multi-client ownership, and llama.cpp/OpenVINO backends behind one gRPC contract.
- **[The editor agent](/docs/rnd/editor-agent/)** — a VS Code extension that bundled the runtime and registered as a native language-model vendor, and an Agent Client Protocol implementation that made approval a blocking protocol operation in both directions.
- **[Hooks became tools](/docs/rnd/hooks/)** — the external-capability boundary, shipped before the industry settled on a name for it: one repo interface, five wire protocols, OpenAPI tool discovery, and MCP slotting in unchanged.
- **[The HTTP API layer](/docs/rnd/api-layer/)** — `apiframework` plus a from-scratch OpenAPI generator that read typed request/response contracts straight out of route annotations and enforced them in CI.
- **[The UI component library](/docs/rnd/ui-library/)** — `@contenox/ui`, a versioned, Storybook-catalogued React design system spanning chat, terminal, and visual workflow components.
- **[Bob: the hosted document & search dashboard](/docs/rnd/bob/)** — a multi-tenant SaaS dashboard with pluggable source connectors, real-embedding semantic search, and a one-click hosted-apps catalog.
- **[runnerd: the self-hosted simulation runner](/docs/rnd/runner/)** — an outbound-only agent that ran branching, human-and-AI tabletop exercises and shipped an auditable, redaction-safe evidence bundle.
- **[vald-operator](/docs/rnd/vald-operator/)** — a Kubernetes operator provisioning one vector-search cluster per tenant, with its unsafe fields made immutable at the API boundary.
- **[The schema-validated app generator](/docs/rnd/blueprints/)** — a schema-validated page format plus a bounded AI draft-and-repair loop that turned a plain-English ask into a working, edit-preserving app.
- **[The MVP: Telegram and GitHub bots on one chain engine](/docs/rnd/mvp/)** — a Telegram co-pilot and an autonomous GitHub PR bot sharing one lease-based job queue and declarative chain engine, first shipped April 2025.
- **[The generation loop](/docs/rnd/handwritten-vs-generated/)** — the 2025 hand-written codebase and its 2026 AI-assisted successor, measured against each other: size was never the variable, and what changed instead was readability, deletion, and naming.
