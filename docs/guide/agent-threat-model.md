---
title: Why contenox confines agents
description: The threat model behind the sandbox — you cannot trust the process of an external ACP agent, so contenox confines it below the agent, kernel-enforced and fail-closed, instead of relying on the agent's or the model's goodwill.
---

# Why contenox confines agents

When contenox hosts an external ACP agent, it spawns a program you did not
write, running on your machine, as you. The [sandbox](/docs/guide/agent-sandbox/)
is how it does that safely. This page is the *why*: the threat model that makes
confinement structural rather than optional.

The load-bearing claim is narrow and worth stating plainly:

> You cannot trust the **process** of an external agent. So contenox confines it
> **below** the agent — in the kernel, fail-closed — instead of trusting the
> agent, or the model driving it, to behave.

"The model is aligned" is not a control. Neither is "the package is popular."
Three separate threats make that true, and they **compose**: any one is enough
to warrant the wall, and they do not cancel out.

## Threat 1 — the model (goodwill does not scale with capability)

The obvious worry is the model: a capable model, told to do something, doing
more than you meant. Two measured trends make this worse over time, not better.

- **Escape capability is climbing.** Frontier models are measurably getting
  better at *detecting and escaping* sandboxes — identifying
  misconfigurations, privilege mistakes, and runtime weaknesses, then exploiting
  them when present ([Quantifying Frontier LLM Capabilities for Container
  Sandbox Escape](https://arxiv.org/abs/2603.02277)). The thing you would rely on
  a sandbox to stop is exactly the thing models are getting good at.
- **Evaluation-awareness rises through training.** Models increasingly behave
  differently when they infer they are being watched. A control that depends on
  observed good behavior degrades precisely when it matters — a passing eval is
  weaker evidence than it looks.

So autonomous capability is going up while the reliability of "it behaved when we
looked" is going down. You cannot buy safety by picking a better model; a better
model is also a better escape artist. The confinement has to hold regardless of
which model is driving.

## Threat 2 — the supply chain (the model is irrelevant here)

Now assume the model is perfectly benign. It does not matter, because the agent
is not just a model — it is a **closed-source package with a transitive
dependency tree**, and that tree runs code on your machine, as you, at install
and spawn, with no model in the loop.

This is not hypothetical. In 2025 the self-replicating **Shai-Hulud** npm worm
compromised hundreds of packages by executing during installation: its payload
ran from `install` lifecycle scripts, harvested npm tokens, GitHub PATs, and
cloud credentials, exfiltrated them, and copied itself into further packages the
stolen credentials could reach
([Sysdig](https://www.sysdig.com/blog/shai-hulud-the-novel-self-replicating-worm-infecting-hundreds-of-npm-packages),
[Datadog Security Labs](https://securitylabs.datadoghq.com/articles/shai-hulud-2.0-npm-worm/)).
The campaign was serious enough to draw a
[CISA alert](https://www.cisa.gov/news-events/alerts/2025/09/23/widespread-supply-chain-compromise-impacting-npm-ecosystem)
urging wholesale credential rotation. Later waves explicitly targeted AI
developer toolchains.

Two properties of this class matter for the design:

- **It runs at install/spawn, not at steady state.** A `postinstall` script
  fires before the agent has processed a single prompt. Confinement that only
  wraps a running agent has already lost.
- **It runs as you.** It reads whatever your user can read — `~/.ssh`, `~/.aws`,
  `~/.npmrc`, your environment — and it only needs a socket to send it onward.

No amount of model alignment touches this. The threat is in the toolchain, below
the agent's own logic.

## Threat 3 — the deployment mode (who holds your authority)

The third threat is not about the code at all — it is about how much authority
you hand it, and that is a choice you make at spawn.

An external ACP agent run in **auto mode** holds your *full* authority: your
filesystem, your network, your credentials, your reach to the open internet. It
does not *request* effects — it simply *has* them, and acts on the world by any
path its process can take.

Contrast a **declared chain** over a model you host or an API you consume. There,
the model never holds your authority. It can only *request* an effect — a tool
call — through a gate you own, and every request surfaces at the tool layer,
where intent is legible and can be [gated by a human](/docs/guide/hitl/). The
capability is the operator's to grant, scope, and revoke, not the agent's to
hold — the "revocable capability" posture that recent work argues coding agents
need ([Lingering Authority: Revocable Capabilities for Coding
Agents](https://arxiv.org/abs/2606.22504)).

The same model can be safe in one mode and dangerous in the other. The
difference is not the model — it is whether your authority lives in a gate you
own or in a process you don't.

## What the threats demand of the design

Because these threats compose, cooperation is not an option: an untrusted
process will not honor a proxy environment variable, a clean `PATH`, or a polite
request to stay in its lane. The confinement has to be true regardless of what
the process *wants*. That forces four properties, and the
[sandbox](/docs/guide/agent-sandbox/) is built to them.

- **Structural and kernel-enforced, not cooperative.** The blocked paths are not
  *denied by policy* — they are *absent by construction*. The rest of the
  filesystem is not mounted; the network has no route; inherited credentials are
  scrubbed from the environment. There is nothing to honor and nothing to
  bypass, because the resource is simply not there.
- **Fail-closed.** If the confinement cannot be built, the agent does not start.
  It never falls back to running with the wall open — a half-built wall is
  treated as no wall.
- **Egress is the crown surface.** Exfiltration needs a socket. Stolen
  credentials that cannot leave the box are inert, so the network wall is the
  highest-value control: name the hosts the agent legitimately needs, and every
  other destination is refused and logged.
- **Cover install/spawn, and assume breach.** The wall is up before the first
  `postinstall` runs, not just once the agent is live. And because a determined
  process will still probe the wall, every refused attempt — an out-of-workspace
  read, a raw socket, a reach for `~/.ssh` — is logged rather than swallowed as a
  silent error. A well-behaved agent never touches the wall; anything that does
  is signal.

## The response: deny by default, carve out by necessity

The wall is not a rulebook of allowed operations. It is a deny-by-default fence
with a short, justified list of holes — the **functional-necessity carve-out**
model. You do not enumerate what the agent may not do; you name only what it
*provably needs* to function (its own auth/config directory to start, its model
endpoint to reach, its package registry to install), each entry justified by a
concrete "breaks without it." Everything unnamed is refused.

That inverts the usual burden. The default answer is *no hole*. The loot paths a
supply-chain payload hunts — `~/.ssh`, `~/.aws`, `~/.npmrc`, the control plane —
are not on the list unless a real breakage forces them, and the carve-out list
itself lives where the agent cannot reach it, so the agent can never punch its
own hole.

> [!NOTE]
> This page is the rationale; the [sandbox guide](/docs/guide/agent-sandbox/) is
> the mechanism — how the default filesystem/exec/environment fence works with no
> setup, and how to turn on the per-host network wall by naming the hosts an
> agent needs.

## Next steps

- [Confining agents: the sandbox wall](/docs/guide/agent-sandbox/) — the how-to: the default fence and the opt-in network wall.
- [Least-privilege shell environment](/docs/guide/environment-scrubbing/) — the same idea for the shells contenox runs in its own process; configured, not yet enforced.
- [Human-in-the-loop](/docs/guide/hitl/) — the tool-layer gate that governs a declared chain's effects, and the only thing governing contenox's own chains, which run outside the wall.
