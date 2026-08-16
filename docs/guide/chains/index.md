---
title: Chains
description: The artifact an agent declaration compiles into — writing one by hand, the loop it forms, and how chain files are named and resolved.
order: 8
---

# Chains

A chain is what a declaration compiles into: the task graph that actually runs. You rarely write one by hand, but reading one is how you check what an agent will do.

- [**Writing a chain by hand**](/docs/guide/chains/writing-a-chain/) — the format, task by task
- [**The agentic loop**](/docs/guide/chains/agentic-loop/) — the loop as an authored task graph, with its own budget
- [**Naming, roles, and resolution**](/docs/guide/chains/naming/) — which file wins, and where each one is looked up
- [**Request routing**](/docs/guide/chains/routing/) — how a routed workflow executes, once the directory has compiled
