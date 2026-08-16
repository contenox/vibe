---
title: How contenox compares
description: What contenox shares with the coding agents, the three things that are structurally different, what those cost, and which tool to pick.
order: 3
---

# How contenox compares

Before Apache, serving a website meant writing your own server. Everyone hand-rolled HTTP alongside their content, and the good ones were genuinely good. Apache did not win by writing better HTML — it moved the serving into infrastructure you install, and left the HTML to you.

Coding agents are the hand-rolled servers. Aider, OpenCode, Kilo Code and Claude Code each carry their own loop, their own tool gate, their own approval flow, their own session store — welded to their own product. They are good at what they are for.

contenox is the layer underneath. It runs agents you declare, under envelopes you write, and it does not care which of those tools you also use — an ACP client is an ACP client. The question is not which agent writes a better patch. It is what your agents are allowed to do while nobody is watching, and who can answer for it afterwards.

Three things follow from being that layer rather than a tool.

## Three structural differences

### 1. The envelope is a separate artifact from the workflow

In the dedicated coding agents, permission lives in the agent's own configuration surface: Claude Code's settings allowlists and permission modes, Aider's `--yes`, the in-session prompts in OpenCode and Kilo Code. Permission is configuration *of the agent*, and it travels with the agent.

Here it is two files. The **chain** says what happens — tasks, models, per-task tool allowlists, transitions, budgets. The **envelope** says what is permitted — `allow` / `approve` / `deny` rules, argument conditions, compute ceilings, who may answer a question, which binaries an allow rule may run. Declaring an agent writes both, so most of the time you edit one Markdown file and read the pair to see what it meant. Two artifacts either way, versioned separately, and both validated by the same command before anything runs them:

```bash
contenox vet --all      # chains AND hitl-policy files, each with its own validator
```

The envelope wraps the whole tool surface, so it is evaluated before every tool call the harness makes — and it fails closed three ways. A call that matches no rule takes `default_action`, which is `approve` when the field is absent. An unknown `default_action`, or a typo inside a `compute` or `trusted_binaries` block, refuses to load the policy at all rather than silently disarming it. And when the named file cannot be loaded, the fallback is a rule-free policy where every call — including reads — asks a human. A broken envelope stops work; it never quietly widens it.

> **Note:**
> `--auto` removes the gate rather than loosening it: the envelope is not consulted at all on that path. It is the escape hatch for a trusted script, not a permissive policy.

What an operator feels: you can hand someone a chain and keep the policy, or tighten the policy without touching the workflow. They move independently.

```bash
contenox mission fire chain-agent-diffreview "review this diff" --wait               # new workflow, same envelope
contenox mission fire agent-planner "..." --policy hitl-policy-strict.json --wait     # new envelope, same workflow
```

A mission's envelope is a required argument, not a default it inherits — the dispatch refuses without one, and validates the file before the first unit starts.

- [HITL policies](/docs/guide/hitl/) — the envelope format, condition operators, and the shipped presets
- [Writing a chain by hand](/docs/guide/chains/writing-a-chain/) and [chain files: naming, roles, and resolution](/docs/guide/chains/naming/) — the other artifact, and how each is resolved

### 2. Human gates are durable and resumable

A coding agent asks in the session. The question lives in the process that asked it, and closing the terminal ends the question with the process.

An `approve` verdict here records the ask as a durable row and the run **checkpoints and releases its process** — whether or not you are sitting there. The ask stays behind. Any process can answer it:

```bash
contenox approvals list
contenox approvals respond <ask-id> --approve
contenox approvals respond <ask-id> --answer "use the staging database"
```

A different terminal, a different day, a machine that has rebooted since. This is not mission-only plumbing: `chat` and `run` install the same checkpoint saver, so an ordinary interactive turn parks and releases too.

The verdict is recorded once, by a SQL compare-and-swap against the pending row — a second responder is told the ask is already resolved. The checkpoint is then claimed under a lease before the run is rebuilt, and deleted when the run completes, so a second process does not replay it. An expiry can never silently pass a call either: `on_timeout` accepts only `approve` or `deny`, and `allow` is rejected when the policy loads.

The nearest analog is not another coding tool. It is workflow infrastructure — Temporal signals and the durable-execution family. That is the neighbourhood this mechanism belongs to, and it is why a mission can be fired and left.

- [HITL policies](/docs/guide/hitl/) — where the approve tier is authored
- [`contenox approvals`](/docs/reference/contenox-cli/#contenox-approvals) — the durable ask inbox

### 3. Delegation is bounded and attributed

When a mission unit hits something it must not decide alone, it raises a question. By default a **human** answers it — that is what the unit escalated for. An envelope may hand routine questions to the agent that fired the mission, and it must say how many:

```json
{
  "default_action": "approve",
  "rules": [],
  "attention": { "allowAgentAnswers": true, "maxAgentAnswers": 2 }
}
```

Three properties, together, are the uncommon part. The grant is **bounded** — once the budget is spent the question is no longer offered to an agent and waits for a person, and `0` does not mean unlimited. The count is **durable and actor-aware** — it is re-derived from the stored asks, so a restart does not refill it, and your own answers do not consume it. And every answer is **attributed**: the durable ask records who answered and whether it was a person or an agent.

Confidence-gated auto-approval exists in other tools. A separation of powers over *durable* asks, with an explicit budget on how much judgment may be delegated and a record of who spent it, we have not found elsewhere.

- [Who may answer a unit's question](/docs/guide/hitl/#who-may-answer-a-subagent-attention) — the `attention` block and each preset's stance

## Also uncommon

Supporting differences — less load-bearing than the three above, still rare:

- **The chain is a state machine you author**, not one loop configured by flags. Classify, work, recover, report: the shipped agent chains open with a `route` task, run a bounded tool loop, fall into a recovery loop with a smaller budget, and end in a task with `"tools": []` that can only report. Per-task tool allowlists, the chain's own `tools_policies` command allow/deny lists (a separate thing from the envelope — these bound what a task may ask for, not what is permitted), `retry_policy` with backoff and jitter, and `edge_traversed_at_least` branches that bound a loop by counting edge traversals — all visible JSON keys. See [the agentic loop](/docs/guide/chains/agentic-loop/).
- **Compute bounds are policy data**, carried in the envelope rather than in the workflow. `maxTurns` and `maxTokens` are enforced host-side in the drive loop; `modelAllowlist` and `backendAllowlist` are enforced at the one seam where a model and backend are actually chosen, covering chat, prompt, streaming and embedding calls alike — so a unit cannot switch itself onto a model you did not name. Matching there is exact rather than normalized, deliberately, because a security boundary must not silently widen. See [compute and attention bounds](/docs/guide/sovereignty/#compute-and-attention-bounds).
- **A structural shell analyzer.** A command line is parsed as a syntax tree (parser only, no interpreter) rather than matched as a string, under an explicit monotonicity contract: structure may name a command the tokenizer could not see, and may upgrade an ask to an allow only inside an audited set of node kinds — it may never widen an allow or narrow a deny. Unparseable input, a non-literal word, or a shell the parser does not read keeps the tokenizer's verdict. [Trusted binaries](/docs/guide/confinement/trusted-binaries/) closes the gap on the other side, because an allow rule pins a command *name* and `PATH` decides what a name means: a declaration pins that name to an absolute real path and a SHA256, and can only ever withdraw an allow.
- **Path containment is one shared mechanism**, not a check each tool reimplements. A single package resolves symlinks and decides whether a candidate path lies under a root, and the filesystem and git tools route through it — so an escape is refused structurally rather than by string-prefix comparison.
- **Events fire chains through declared triggers.** Internal domain events land in a durable append-only log; operator-authored `trigger-*.json` files bind an event type to a chain, and each (trigger, event) pair fires at most once across processes and restarts. Opt-in and beta. See [Events & triggers](/docs/guide/events/).

## Where it fits

Reach for contenox when the work is **governed, unattended, or repeatable**. When a run has to stop for a named human at a named point and survive the wait. When the person who owns the permissions is not the person who wrote the workflow. When the same thing must run identically in CI, in a cron job, and on your laptop. When you need to say afterwards, from a file rather than from memory, exactly what an agent was permitted to do last Tuesday.

A coding session is one workload on that harness. Run whichever coding agent you like on the same repository — they are a layer, not an opponent.

## Next

- [Declaring agents](/docs/guide/agents/) — the file an agent is
- [Core concepts](/docs/guide/concepts/) — agents, chains, tasks, tools, transitions
- [HITL policies](/docs/guide/hitl/) — the envelope in full
- [The agentic loop](/docs/guide/chains/agentic-loop/) — the loop as an authored task graph
- [Request routing](/docs/guide/chains/routing/) — one prompt, several specialist loops, each with its own tool scope and budget
- [AI sovereignty & the EU AI Act](/docs/guide/sovereignty/) — hosting, state, and oversight controls you own
