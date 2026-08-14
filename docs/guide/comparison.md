---
title: How contenox compares
description: What contenox shares with the coding agents, the three things that are structurally different, what those cost, and which tool to pick.
---

# How contenox compares

The loop is not the difference. What the loop is allowed to do while nobody is watching — that is the difference.

This page is written the way an evaluation actually goes: everything contenox has in common with the dedicated coding agents first, then the three things that are built differently, then what those cost, then which tool to reach for.

## What it shares with the coding agents

A chat loop with tools. Tool calling. MCP servers. Provider switching. Editor integration over ACP. Sessions on local disk. A terminal UI. Aider, OpenCode, Kilo Code and Claude Code all ship that set, each in its own shape, and none of it is an argument for contenox.

Concretely, the overlap:

| Capability | In contenox |
|---|---|
| Tool-using chat loop | `chat_completion` + `execute_tool_calls` tasks with a back-edge — [the agentic loop](/docs/guide/agentic-loop/) |
| MCP servers | `contenox mcp add <name> <url>` — [MCP integration](/docs/integrations/tools/mcp/) |
| Provider switching | `contenox backend add` for Ollama, vLLM, OpenAI, Anthropic, Gemini, Vertex AI, Bedrock; routing is config, not code |
| Editor sessions | `contenox acp` over stdio — [Zed](/docs/integrations/editors/zed/), [JetBrains](/docs/integrations/editors/jetbrains/), [AionUi](/docs/integrations/editors/aionui/), [OpenClaw](/docs/integrations/editors/openclaw/) |
| Local session state | SQLite at `~/.contenox/local.db`; no account — the [relay](/docs/guide/pairing/) is opt-in and never contacted unpaired |
| Terminal chat | `contenox chat`, or a bare `contenox "..."` — session-backed, history carried across invocations |

An ACP editor session is a real coding session, not a demo shell: it routes coding turns into their own loop with its own budget, `local_shell` is available under policy, and the filesystem tools are editor-grade — `read_file`, `read_file_range`, `write_file`, `edit_file`, `sed`, `grep`, `list_dir`, `stat_file`, `delete_file`, with a read-before-write gate that refuses to mutate a file the model has not read. The `/mission` slash command works there too.

And the honest half of that: for **pure coding ergonomics** — repository mapping, diff application, edit formats, the accumulated craft of getting a model to land a patch on the first try — the dedicated coding agents are more refined. That is what they are for, and they have spent their whole existence on it. Table stakes are table stakes; they are not the argument.

## Three structural differences

### 1. The envelope is a separate artifact from the workflow

In the dedicated coding agents, permission lives in the agent's own configuration surface: Claude Code's settings allowlists and permission modes, Aider's `--yes`, the in-session prompts in OpenCode and Kilo Code. Permission is configuration *of the agent*, and it travels with the agent.

Here it is a second file. The **chain** says what happens — tasks, models, per-task tool allowlists, transitions, budgets. The **envelope** says what is permitted — `allow` / `approve` / `deny` rules, argument conditions, compute ceilings, who may answer a question, which binaries an allow rule may run. Two artifacts, authored separately, versioned separately, and both validated by the same command before anything runs them:

```bash
contenox vet --all      # chains AND hitl-policy files, each with its own validator
```

The envelope wraps the whole tool surface, so it is evaluated before every tool call the harness makes — and it fails closed three ways. A call that matches no rule takes `default_action`, which is `approve` when the field is absent. An unknown `default_action`, or a typo inside a `compute` or `trusted_binaries` block, refuses to load the policy at all rather than silently disarming it. And when the named file cannot be loaded, the fallback is a rule-free policy where every call — including reads — asks a human. A broken envelope stops work; it never quietly widens it.

> **Note:**
> `--auto` removes the gate rather than loosening it: the envelope is not consulted at all on that path. It is the honest escape hatch for a trusted script, not a permissive policy.

What an operator feels: you can hand someone a chain and keep the policy, or tighten the policy without touching the workflow. They move independently.

```bash
contenox run --chain ./chain-agent-diffreview.json "review this diff"        # new workflow, same envelope
contenox mission fire agent-planner "..." --policy hitl-policy-strict.json   # new envelope, same workflow
```

A mission's envelope is a required argument, not a default it inherits — the dispatch refuses without one, and validates the file before the first unit starts.

- [HITL policies](/docs/guide/hitl/) — the envelope format, condition operators, and the shipped presets
- [Your first chain](/docs/guide/first-chain/) and [chain files: naming, roles, and resolution](/docs/guide/chain-naming/) — the other artifact, and how each is resolved

### 2. Human gates are durable and resumable

A coding agent asks in the session. The question lives in the process that asked it, and closing the terminal ends the question with the process.

An `approve` verdict here parks the call for a short window in case you are sitting there — and when that window passes, the run **checkpoints and releases its process**. The ask stays behind as a durable row. Any process can answer it:

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

- [Who may answer a unit's question](/docs/guide/hitl/#who-may-answer-a-units-question-attention) — the `attention` block and each preset's stance
- [The attention oracle (beta)](/docs/use-cases/auto-attention/) — a driver that spends that budget on routine questions so unattended runs finish

## Also uncommon

Supporting differences — less load-bearing than the three above, still rare:

- **The chain is a state machine you author**, not one loop configured by flags. Classify, work, recover, report: the shipped agent chains open with a `route` task, run a bounded tool loop, fall into a recovery loop with a smaller budget, and end in a task with `"tools": []` that can only report. Per-task tool allowlists, the chain's own `tools_policies` command allow/deny lists (a separate thing from the envelope — these bound what a task may ask for, not what is permitted), `retry_policy` with backoff and jitter, and `edge_traversed_at_least` branches that bound a loop by counting edge traversals — all visible JSON keys. See [the agentic loop](/docs/guide/agentic-loop/).
- **Compute bounds are policy data**, carried in the envelope rather than in the workflow. `maxTurns` and `maxTokens` are enforced host-side in the drive loop; `modelAllowlist` and `backendAllowlist` are enforced at the one seam where a model and backend are actually chosen, covering chat, prompt, streaming and embedding calls alike — so a unit cannot switch itself onto a model you did not name. Matching there is exact rather than normalized, deliberately, because a security boundary must not silently widen. See [compute and attention bounds](/docs/guide/sovereignty/#compute-and-attention-bounds).
- **A structural shell analyzer.** A command line is parsed as a syntax tree (parser only, no interpreter) rather than matched as a string, under an explicit monotonicity contract: structure may name a command the tokenizer could not see, and may upgrade an ask to an allow only inside an audited set of node kinds — it may never widen an allow or narrow a deny. Unparseable input, a non-literal word, or a shell the parser does not read keeps the tokenizer's verdict. [Trusted binaries](/docs/guide/trusted-binaries/) closes the gap on the other side, because an allow rule pins a command *name* and `PATH` decides what a name means: a declaration pins that name to an absolute real path and a SHA256, and can only ever withdraw an allow.
- **Path containment is one shared mechanism**, not a check each tool reimplements. A single package resolves symlinks and decides whether a candidate path lies under a root, and the filesystem and git tools route through it — so an escape is refused structurally rather than by string-prefix comparison.
- **Events fire chains through declared triggers.** Internal domain events land in a durable append-only log; operator-authored `trigger-*.json` files bind an event type to a chain, and each (trigger, event) pair fires at most once across processes and restarts. Opt-in and beta. See [Events & triggers](/docs/guide/events/).

## What this costs

- **The weakest surfaces are the ones you meet first.** Setup, `init`, and the first-run path are the least-polished parts of contenox; the differentiators sit behind them. A first hour spent on provider config is not the hour that shows you why this exists.
- **Kernel-enforced confinement is Linux-only.** The [sandbox](/docs/guide/agent-sandbox/) uses Landlock and Linux namespaces. Off Linux it refuses to build rather than handing back an unconfined command — fail-closed, but unavailable. It also confines *foreign* agents, and registering one is not exposed yet, so nothing on a stock install takes that path. What governs contenox's own chains is the tool gate and the envelope, not a kernel wall.
- **A few envelope fields declare more than the shipped hosts enforce.** `maxToolCalls` is validated but not enforced by any shipped host: its one enforcement seam is the unattended permission answerer, and nothing shipped wires that. `maxTokens` is best-effort — it applies when a unit reports usage and is inert when the provider reports none. Nothing catches either at author time: `contenox vet` is silent about both, and its `WARN` lines cover only trusted-binary declarations that no longer match this host. What carries the disclosure is the `//compute-fields` note each shipped preset keeps in its own file, which is the honest version of the problem but not a fix for it.
- **Ask expiry is applied lazily.** A durable ask carries a deadline — an hour by default — and `on_timeout` defaults to deny. But nothing sweeps in the background: there is no daemon, so the deadline is applied when the inbox is listed, and until then a late answer still resumes the run. The no-daemon stance is deliberate; the consequence is still worth knowing before you rely on a deadline.
- **The payoff is on day thirty, not day one.** On day one this is a chat loop with tools, and the dedicated coding agents polish that better. Separate envelopes, durable approvals, and delegation budgets start earning on the day you hand a workflow to somebody else, or leave one running overnight, or have to explain to a third party what an agent was permitted to do last Tuesday.

## Which one to pick

If what you want is the best pure coding ergonomics available today — repository mapping, diff application, edit formats, the coding-specific interaction craft — use a dedicated coding agent. Aider, OpenCode, Kilo Code and Claude Code are further along there, contenox does not try to beat them at it, and nothing stops you from running one of them and contenox on the same repository. They are a layer, not an opponent.

Reach for contenox when the work is **governed, unattended, or repeatable** and the coding agent is one workload among several. When a run has to stop for a named human at a named point and survive the wait. When the person who owns the permissions is not the person who wrote the workflow. When the same thing must run identically in CI, in a cron job, and on your laptop. When you need to say afterwards, from a file rather than from memory, exactly what the agent was allowed to do. A coding session is one shape that work takes here, but it is a workload on the harness, not the reason the harness exists.

## Next

- [Core concepts](/docs/guide/concepts/) — chains, tasks, tools, transitions
- [HITL policies](/docs/guide/hitl/) — the envelope in full
- [The agentic loop](/docs/guide/agentic-loop/) — the loop as an authored task graph
- [Request routing](/docs/guide/request-routing/) — one prompt, several specialist loops, each with its own tool scope and budget
- [AI sovereignty & the EU AI Act](/docs/guide/sovereignty/) — hosting, state, and oversight controls you own
