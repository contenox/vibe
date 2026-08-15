---
title: The oracle
description: An adjudicating agent that answers a subagent's routine asks — questions, and optionally gated tool calls — so unattended runs finish; consequential asks still wait for you.
---

# The oracle

Unattended subagents stall. You start a plan, walk away, and come back to a run parked on an ask its own intent already answers — "which directory holds the docs?" on a subagent whose intent names the docs — or on a gated tool call nobody was there to approve. The **oracle** keeps the shift moving: it reviews each ask a subagent raises, rules on the routine ones so the run continues, and leaves everything else exactly where it was — a durable ask waiting for a human.

`wait` is always the safe verdict, and the durable record shows exactly who decided what.

## What it may rule on

| Ask kind | Raised when | Verdicts |
|---|---|---|
| **question** | the subagent calls `mission_ask_attention` | `answer`, `wait` |
| **gated tool call** | the subagent makes a call the envelope put on the `approve` tier | `approve`, `deny` (with guidance), `wait` |

The second is off by default and takes two separate grants to turn on — see [Turning it on](#turning-it-on). Letting a model release a gated call is a bigger grant than letting it answer a question, so it is never implied by the first.

## Turning it on

The oracle is a configured default, not a flag you remember to pass:

```bash
# Which oracle. Setting this is what turns it on; unset means no oracle at all.
contenox config set default-oracle-chain chain-oracle-default.json

# Optional: the envelope the oracle chain itself runs under.
contenox config set default-oracle-policy hitl-policy-oracle.json

# Optional: let it rule on gated TOOL CALLS, not just questions.
contenox config set oracle-approves-tool-calls true
```

Every value is overridable per run, the way every contenox default is:

```bash
contenox acp --oracle chain-oracle-default.json --oracle-approves-tool-calls
contenox acp --oracle off          # this run only, no oracle
```

The oracle mounts on the **ACP host**, which is where subagents actually come from: `/plan` and the `mission_start` tool. Its lifetime is the host's — when the editor session ends, the oracle is gone with it.

## The two grants

Turning the oracle on is not enough to let it release a gated call. The **subagent's own envelope** decides that, and it is a separate file from the oracle's:

```json
{
  "attention": {
    "allowAgentAnswers": true,   "maxAgentAnswers": 3,
    "allowAgentApprovals": true, "maxAgentApprovals": 10
  }
}
```

Both have to agree — the config key says *this host has an oracle that may rule on calls*, the envelope says *this subagent's calls may be ruled on*. Either one off means the call waits for you.

The counts are durable and actor-aware: a restart does not refill them, your own verdicts do not consume them, and the counting and the write happen in one statement, so a firing session's agent and the oracle answering the same mission concurrently still cannot overrun the budget.

## What happens on an ask, step by step

1. The subagent raises the ask. It becomes a durable row immediately (`contenox approvals list` shows it), and the subagent parks on it for a short window before checkpointing.
2. The ask is offered to the oracle **in-process, before a human sees it**.
3. The oracle runs its chain with the ask as input: the ask kind, the subagent's **intent**, and — for a gated call — the tool, its arguments, and the rule that gated it. The model holds exactly one tool, `oracle.submit_verdict`, and one job: judge it against that intent and submit one verdict. The loop is budgeted and self-correcting — a malformed call or a chat-text reply gets a machine-register correction and a bounded retry.
4. A verdict goes through the **service layer**, under the subagent envelope's bounds, recorded as `answeredBy`/`decidedBy: "oracle"`. The parked subagent's poll picks the resolved row up and the run continues — in the window, with no checkpoint ever created. Past the window, the durable resume path picks it up instead, exactly as a human's late answer would.

## Everything that is not a verdict leaves the ask alone

`wait`, a chain error, a spent budget, a malformed call after its retries, an envelope that forbids it — every one of these changes nothing. The ask stays pending and takes the untouched normal path: your terminal, your editor's card, or the `on_timeout` verdict.

**The oracle can only ever be faster than you, never more permissive than the envelope.** It cannot widen a rule, cannot reach a tool the policy denies, and cannot exceed the counts.

## Why a denial carries guidance

A refused tool call reaches the model as a bare rejection — the protocol has no free-text channel on a permission response. A subagent that only ever sees "rejected" circles: it retries the same call, or gives up.

So a `deny` verdict takes an optional `guidance` — one sentence naming what to do **instead**:

```json
{"verdict": "deny", "guidance": "write under ./out, not /tmp", "askId": "..."}
```

That redirect is recorded on the ask, and the runtime's next prompt turn to the subagent carries every redirect it collected. The subagent learns *why* it was blocked and what to do about it, on the turn after it happened.

## What it costs

One model call per ask reviewed. The oracle chain is small — one tool, a hard round budget — but it is not free, and it runs on every ask the subagent raises, including the ones it will decline to decide.

If a subagent raises enough asks for that to matter, the oracle is treating a symptom. A subagent asking constantly is usually one whose **intent** is too vague or whose **envelope** gates the wrong things. Fix those first; the oracle is for the residue, not the flood.

## The oracle's own envelope

`hitl-policy-oracle.json` is `default_action: deny` with exactly two allows — `oracle.submit_verdict` and `oracle.verdict_state`, both in-process. No shell, no filesystem, no network, and no `command_prefix_allowlist` (a prefix allowlist pins a *name*, and `PATH` decides what a name resolves to).

Its own `attention` block is human-only on both halves: the oracle never adjudicates its own asks. An ask the oracle chain raises waits for a person, which is also what stops it from being offered its own question in a loop.

## Reading what it did

```bash
contenox approvals list            # what is still pending
contenox mission asks              # narrowed to open missions
contenox mission reports <id>      # what the subagent actually did
```

Every resolved row records who decided it. An oracle verdict is never anonymous and never indistinguishable from yours.

## Next

- [Missions](/docs/guide/missions/) — the substrate every subagent runs on
- [HITL policies](/docs/guide/hitl/#who-may-answer-a-subagent-attention) — the `attention` bounds in full
- [Events & triggers (beta)](/docs/guide/events/) — buzzing your phone on an ask the oracle left for you
