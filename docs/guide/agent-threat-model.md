---
title: Why contenox confines agents
description: The threat model behind the sandbox — you cannot trust the process of an external ACP agent, so contenox confines it below the agent, kernel-enforced and fail-closed, instead of relying on the agent's or the model's goodwill.
---

# Why contenox confines agents

When contenox hosts an external ACP agent, it spawns a program you did not write, running on your machine, as you. The [sandbox](/docs/guide/agent-sandbox/) is how it does that safely. This page covers the threat model: why confinement is structural rather than optional.

> You cannot trust the **process** of an external agent. So contenox confines it **below** the agent — in the kernel, fail-closed — instead of trusting the agent, or the model driving it, to behave.

"The model is aligned" is not a control, and neither is "the package is popular." Three separate threats justify the wall, and they compose: any one alone is sufficient, and they do not cancel each other out.

## Threat 1 — the model

A capable model, told to do something, may do more than intended. Two trends make this worse over time:

- **Escape capability is rising.** Frontier models are measurably improving at detecting and escaping sandboxes — identifying misconfigurations, privilege mistakes, and runtime weaknesses, then exploiting them ([Quantifying Frontier LLM Capabilities for Container Sandbox Escape](https://arxiv.org/abs/2603.02277)).
- **Evaluation-awareness rises through training.** Models increasingly behave differently when they infer they are being observed, which weakens the evidential value of a passing eval.

Autonomous capability is rising while the reliability of "it behaved when we looked" is falling. A better model is also a better escape artist, so the confinement has to hold regardless of which model drives the agent.

## Threat 2 — the supply chain

Even a benign model does not help here, because the agent is not just a model — it is a closed-source package with a transitive dependency tree, and that tree runs code on your machine, as you, at install and spawn, with no model in the loop.

In 2025 the self-replicating **Shai-Hulud** npm worm compromised hundreds of packages by executing during installation: its payload ran from `install` lifecycle scripts, harvested npm tokens, GitHub PATs, and cloud credentials, exfiltrated them, and propagated into further packages the stolen credentials could reach ([Sysdig](https://www.sysdig.com/blog/shai-hulud-the-novel-self-replicating-worm-infecting-hundreds-of-npm-packages), [Datadog Security Labs](https://securitylabs.datadoghq.com/articles/shai-hulud-2.0-npm-worm/)). The campaign drew a [CISA alert](https://www.cisa.gov/news-events/alerts/2025/09/23/widespread-supply-chain-compromise-impacting-npm-ecosystem) urging wholesale credential rotation. Later waves targeted AI developer toolchains specifically.

Two properties matter for the design:

- **It runs at install/spawn, not at steady state.** A `postinstall` script fires before the agent processes a single prompt. Confinement that only wraps a running agent has already lost.
- **It runs as you.** It reads whatever your user can read — `~/.ssh`, `~/.aws`, `~/.npmrc`, your environment — and needs only a socket to send it onward.

No amount of model alignment addresses this threat, since it lives in the toolchain, below the agent's own logic.

## Threat 3 — the deployment mode

The third threat is about how much authority you hand the agent, which is a choice made at spawn time.

An external ACP agent run in **auto mode** holds your full authority: your filesystem, your network, your credentials, your reach to the open internet. It does not request effects — it has them, and acts on the world by any path its process can take.

Contrast a declared chain over a model you host or an API you consume. There, the model never holds your authority. It can only request an effect — a tool call — through a gate you own, and every request surfaces at the tool layer, where it can be [gated by a human](/docs/guide/hitl/). The capability is the operator's to grant, scope, and revoke, not the agent's to hold — the "revocable capability" posture recent work argues coding agents need ([Lingering Authority: Revocable Capabilities for Coding Agents](https://arxiv.org/abs/2606.22504)).

The same model can be safe in one mode and dangerous in the other. The difference is whether your authority lives in a gate you own or in a process you don't.

## What the threats demand of the design

Because these threats compose, cooperation is not an option: an untrusted process will not honor a proxy environment variable, a clean `PATH`, or a request to stay in its lane. The confinement has to hold regardless of what the process wants. That forces four properties, which the [sandbox](/docs/guide/agent-sandbox/) is built to:

- **Structural and kernel-enforced, not cooperative.** Blocked paths are absent by construction, not denied by policy: the rest of the filesystem is not mounted, the network has no route, inherited credentials are scrubbed from the environment. There is nothing to honor and nothing to bypass.
- **Fail-closed.** If the confinement cannot be built, the agent does not start. It never falls back to running with the wall open.
- **Egress is the highest-value control.** Exfiltration needs a socket. Stolen credentials that cannot leave the box are inert, so the network wall matters most: name the hosts the agent legitimately needs, and every other destination is refused and logged.
- **Cover install/spawn, and assume breach.** The wall is up before the first `postinstall` runs, not only once the agent is live. Every refused attempt — an out-of-workspace read, a raw socket, a reach for `~/.ssh` — is logged rather than swallowed silently.

## The response: deny by default, carve out by necessity

The wall is not a rulebook of allowed operations. It is a deny-by-default fence with a short, justified list of holes — the functional-necessity carve-out model. You do not enumerate what the agent may not do; you name only what it provably needs to function (its own auth/config directory to start, its model endpoint, its package registry), each entry justified by a concrete "breaks without it." Everything unnamed is refused.

The loot paths a supply-chain payload hunts — `~/.ssh`, `~/.aws`, `~/.npmrc`, the control plane — are not on the list unless a real breakage forces them, and the carve-out list itself lives where the agent cannot reach it, so the agent can never punch its own hole.

> **Note:**
> This page is the rationale; the [sandbox guide](/docs/guide/agent-sandbox/) is the mechanism — how the default filesystem/exec/environment fence works with no setup, and how to turn on the per-host network wall by naming the hosts an agent needs.

## Next steps

- [Confining agents: the sandbox wall](/docs/guide/agent-sandbox/) — the default fence and the opt-in network wall.
- [Least-privilege shell environment](/docs/guide/environment-scrubbing/) — the same idea for the shells contenox runs in its own process; configured, not yet enforced.
- [Human-in-the-loop](/docs/guide/hitl/) — the tool-layer gate that governs a declared chain's effects, and the only thing governing contenox's own chains, which run outside the wall.
- [AI sovereignty & the EU AI Act](/docs/guide/sovereignty/) — how this confinement posture, local state, and authored oversight fit together as an operator-controlled deployment.
