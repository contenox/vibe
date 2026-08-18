---
title: "Scoping what an agent may do"
description: Six declarations decide what an LLM can reach, touch, spend and act on, and which of those need a human first. Each is a file, each fails closed, and none of them depends on the model behaving.
order: 2
---

# Guardrails — scoping what an agent may do

A model that only writes text needs its output checked. A model that runs shell
commands, edits files and calls APIs needs its *actions* scoped, before they
happen, by something that is not the model.

In contenox that scope is six declarations. None of them is a setting you tick;
each is a file you write, diff and review like any other change.

| What it decides | Where you declare it |
|---|---|
| Which model answers | `model:` in the [agent declaration](/docs/guide/agents/); `execute_config.model` / `provider` in an authored chain |
| Which tools exist at all | `tools:` in the declaration; `execute_config.tools` allowlist in an authored chain |
| Where it may act | the instance's one workspace, and the [sandbox](/docs/guide/confinement/sandbox/) |
| What runs, asks, or is refused | the envelope — [HITL policy](/docs/guide/hitl/) |
| What content gets through | a `route` task — [moderation gate](/docs/use-cases/moderation-gate/) |
| What it may spend | `compute` bounds in the envelope |

## 1. Which model answers

A declaration's `model:` pins the model for that agent; routing stays on your
configured default when the field is absent. In an authored chain a task names
its model and provider per step. Nothing negotiates that at runtime, and an
envelope can pin it further: `modelAllowlist` and `backendAllowlist` in the
`compute` block mean a unit cannot switch to a model you did not name — see
[sovereignty](/docs/guide/sovereignty/) for why that matters when the inference
has to stay on your hardware.

## 2. Which tools exist at all

A declaration's `tools:` line is the allowlist most agents use. Omitting it
inherits every tool, so name them to narrow it.

Behind that, and in a chain you write yourself, the field is
`execute_config.tools` — an allowlist whose default is the important part:

> Absent or `null` = none. The task has no tools until this field explicitly
> grants some.

`[]` is no tools, `["*"]` is all, `["*","!local_shell"]` is all-except. There is
also `hide_tools` to suppress specific tools from both the registry and any
client-passed set, and `tools_policies` to constrain a provider before it runs —
`local_shell: { "_allowed_commands": "git,go,ls" }`.

A tool the task was never granted is not a tool the model can be argued into
calling.

## 3. Where it may act

An instance serves exactly one workspace, fixed when it was launched: the
directory `beam` or `run` started in, the path `serve` was given, the project an
editor opened. Its sessions run there and nowhere else — never in a directory a
client asked for, and never in the runtime's own config, database or policies.
See [workspace authority](/docs/reference/contenox-cli/#workspace-authority).

Every agent-reachable shell gets a
[scrubbed environment](/docs/guide/confinement/environment/), so credentials in
your shell are not credentials in the agent's. The reasoning is in the
[threat model](/docs/guide/confinement/why/): the process on the other end of
an external agent connection is not one you can trust.

## 4. What runs, asks, or is refused

The envelope is evaluated before **every** tool call. Three verdicts:

- `allow` — runs silently
- `approve` — pauses and waits for a person
- `deny` — refused

A call no rule matches takes `default_action`. Set that to `approve` and
anything you did not think of asks instead of proceeding — which is the point of
declaring rules rather than enumerating threats.

Rules match on the tool and its arguments, with conditions like
`command_prefix_allowlist`, so `git` and `go test` can pass while everything
else on the same shell stops. The full grammar is in
[HITL policies](/docs/guide/hitl/), and the format has a
[published JSON Schema](/schema/hitl-policy-v1.schema.json) you can point your
editor at.

**It fails closed in three directions.** An unknown `default_action`, or a typo
inside `compute` or `trusted_binaries`, refuses to load the policy rather than
silently disarming it. A policy file that cannot be read falls back to asking
about everything, including reads. A broken envelope stops work; it never
quietly widens it.

## 5. What content gets through

Content moderation is not a subsystem here, it is a task. A `route` task
classifies with a cheap model and the transition sends the request to the real
chain or to a rejection:

```json
{ "id": "moderate", "handler": "route",
  "transition": { "branches": [
    { "operator": "equals", "when": "safe",   "goto": "simple-chat" },
    { "operator": "equals", "when": "unsafe", "goto": "reject_request" },
    { "operator": "default", "goto": "simple-chat" } ] } }
```

That is [the moderation gate](/docs/use-cases/moderation-gate/), shipped as
`examples/simple-chat-with-moderation.json`. Put the same task *after* the
generating task instead of before it and you have output moderation — the
mechanism does not change, only its position. You pick the classifier model, and
you can read what it decides on.

## 6. What it may spend

`compute` caps turns, tool calls and tokens. A unit that crosses a bound ends as
`stuck` rather than running on. `attention` decides who may answer a unit's
question — by default a human, never the agent itself.

## None of this asks the model to behave

Every declaration above is enforced outside the model, before the effect lands.
A tool that was never granted cannot be argued into existence; a path outside
the instance's workspace is not reachable by a better prompt; a call the
envelope denies does not run. That is what makes them guardrails rather than
instructions.

## Next

- [HITL policies](/docs/guide/hitl/) — the envelope format in full.
- [The agent sandbox](/docs/guide/confinement/sandbox/) — the filesystem and exec fence.
- [The threat model](/docs/guide/confinement/why/) — why the fence exists.
- [Nested permission bomb](/docs/use-cases/nested-permission-bomb/) — why
  inheriting your own permissions is the bug.
