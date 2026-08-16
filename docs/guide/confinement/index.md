---
title: Confining agents
description: Why an agent is confined, how to scope what it may reach, and the mechanisms that hold the line — the sandbox wall, trusted binaries, and a least-privilege environment.
order: 10
---

# Confining agents

The envelope decides what stops for a human. These decide what is reachable at all.

- [**Why contenox confines agents**](/docs/guide/confinement/why/) — the threat model this answers
- [**Scoping what an agent may do**](/docs/guide/confinement/guardrails/) — narrowing a toolset before anything runs
- [**The sandbox wall**](/docs/guide/confinement/sandbox/) — Landlock and namespaces around a foreign agent
- [**Trusted binaries**](/docs/guide/confinement/trusted-binaries/) — pinning what a shell command is allowed to be
- [**Least-privilege shell environment**](/docs/guide/confinement/environment/) — what a spawned command inherits
