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
| Where it may act | the instance's one workspace, the [sandbox](/docs/guide/confinement/sandbox/), and the envelope's `files.*` path lists |
| What runs, asks, or is refused | the envelope's capability axes — [HITL policy](/docs/guide/hitl/) |
| What content gets through | a `route` task — [moderation gate](/docs/use-cases/moderation-gate/) |
| What it may spend | `compute` bounds in the envelope |

Four of the six are the envelope, which is why it has a vocabulary of its own: a
named `[envelopes.<name>]` section in
[`agents.toml`](/docs/reference/agents-config/#envelopesname), transpiled into
the policy the approval engine evaluates.

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

`[]` is no tools, `["*"]` is all, `["*","!local_shell"]` is all-except. The
vocabulary is exactly four things, and `*` is the one worth being precise about:

| Entry | Meaning |
|---|---|
| `"*"` | **every connected toolset, with no exceptions** — including the `native-` in-process toolsets and the `decl-` sources other agents brought |
| `"!name"` | removes that one toolset; an exclusion beats `*`, whatever order they appear in |
| `"name"` | grants exactly that toolset |
| `[]` (or absent) | grants nothing |

A `native-` or `decl-` prefix is a namespace that stops a declared source from
colliding with an in-process toolset. It is not a hidden exclusion: `*` admits
those rows like any other, and `"!native-git"` is how you drop one. Narrowing is
something you write, not something the runtime does for you.

There is also `hide_tools` to suppress specific tools from both the registry and
any client-passed set, and `tools_policies` to constrain a provider before it
runs — `local_shell: { "_allowed_commands": "git,go,ls" }`.

A tool the task was never granted is not a tool the model can be argued into
calling.

Reachable is not the same as permitted. A tool you connected — an MCP server, an
OpenAPI subset — matches no shipped rule, so it falls to the envelope's
`default_action` and asks on every call until you name it:

```toml
[envelopes.mine.tools]
"github.*" = "approve"
"tavily.search" = "allow"
```

## 3. Where it may act

An instance serves exactly one workspace, fixed when it was launched: the
directory `beam` or `run` started in, the path `serve` was given, the project an
editor opened. Its sessions run there and nowhere else — never in a directory a
client asked for, and never in the runtime's own config, database or policies.
See [workspace authority](/docs/reference/contenox-cli/#workspace-authority).

Inside that workspace the envelope narrows further. The two file axes take path
lists, and a `deny_paths` glob is emitted **ahead** of the grant it carves out
of, so a directory can be unreachable while the tree around it is readable:

```toml
[envelopes.mine.files.read]
grant = "allow"
approve_paths = ["**/{*.pem,*.key,.env,.env.*}"]
```

The shipped envelopes use this for the **credential quarantine** — key stores,
keyrings, wallets, browser profiles, shell history — which rides on `read_only`,
the base every other posture extends, and is therefore in force under the most
permissive posture exactly as under the strictest. A declaration cannot name
those paths, so it cannot consent to them either.

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

You write those verdicts against capability **axes**, and the runtime compiles
them into the rules the engine matches:

```toml
[envelopes.mine]
files.read = "allow"
files.write = "approve"
shell = "deny"
missions.fire = "allow"
```

An axis you leave unset emits no rule and takes `default_action`. Set that to
`approve` and anything you did not think of asks instead of proceeding — which
is the point of declaring capabilities rather than enumerating threats.

The `shell` axis is the one with tiers, because "which commands" is never one
answer: a `blacklist` that cannot be reached past, a `substitution` verdict
judged before any verb is trusted, a `prefix_allowlist` that grants, and an
`ask_always` list that claws back. So `go test` and `ls` can pass while `rm` and
`sudo` still ask and `mkfs` is refused, all on the same shell.

An `approve` grant stops for a person, and how long it stops is also yours to
write. Any grant takes a `timeout` and an `on_timeout` in its table form:

```toml
[envelopes.mine]
files.write = { grant = "approve", timeout = "30m", on_timeout = "deny" }
```

Nobody answers in thirty minutes, the ask is denied and the run moves on. Write
`timeout = "never"` instead and the ask has no deadline at all: it waits, across
restarts, until somebody answers it. Leave both out and the ask is bounded by
this host's approval ceiling — `contenox config set approval-ceiling
<duration|never>`, seven days until you set it. `deny` is the only `on_timeout`
there is: an ask that allowed itself when nobody answered would bypass the
approval it exists to require, and beside `timeout = "never"` it is refused
outright, since nothing can expire. See
[Bounding the wait](/docs/reference/agents-config/#bounding-the-wait).

The full axis grammar is in
[`[envelopes.<name>]`](/docs/reference/agents-config/#envelopesname), the
compiled format is in [HITL policies](/docs/guide/hitl/), and that format has a
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

The envelope's `compute` block caps turns, tool calls and tokens. A unit that
crosses a bound ends as `stuck` rather than running on. The `missions.answer`
axis decides who may answer a unit's question — by default a human, never the
agent itself — and `attention` puts the numbers on it.

```toml
[envelopes.mine.compute]
max_tool_calls = 300
max_tokens = 2000000
on_exhausted = "finish_stuck"
```

Enforcement differs per field and
[Missions](/docs/guide/missions/#compute-bounds-and-what-actually-enforces-them)
says which is which; the runtime is explicit about it rather than implying
uniform coverage.

## None of this asks the model to behave

Every declaration above is enforced outside the model, before the effect lands.
A tool that was never granted cannot be argued into existence; a path outside
the instance's workspace is not reachable by a better prompt; a call the
envelope denies does not run. That is what makes them guardrails rather than
instructions.

## Next

- [HITL policies](/docs/guide/hitl/) — where an envelope comes from, and what it compiles to.
- [`[envelopes.<name>]`](/docs/reference/agents-config/#envelopesname) — the axis grammar in full.
- [The agent sandbox](/docs/guide/confinement/sandbox/) — the filesystem and exec fence.
- [The threat model](/docs/guide/confinement/why/) — why the fence exists.
- [Nested permission bomb](/docs/use-cases/nested-permission-bomb/) — why
  inheriting your own permissions is the bug.
