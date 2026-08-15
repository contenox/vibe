---
title: "The attention oracle"
description: A second agent that triaged a mission's questions so unattended runs would not stall. Removed 2026-08-14, brought back the same day with a wider contract. What the removal got right, what it got wrong, and what the attempt settled.
---

# The attention oracle

The oracle was a small agent whose only job was reading another agent's
questions and answering the routine ones.

It was removed on 2026-08-14 — about a thousand lines — and brought back the
same day, adjudicating **gated tool calls** as well as questions, mounted from
config rather than a flag. This record keeps both halves, because the removal
was right about the thing it looked at and wrong about the thing it did not.

The shipped design is [The oracle](/docs/use-cases/auto-attention/).

## What the attempt settled

**The verdict contract was the right shape.** One ask in, one verdict out, no
chat text, one tool, a hard round budget. An agent that can only rule cannot
wander into doing the work it was asked about. That constraint held for the
whole life of the feature, survived the removal, and is what the wider contract
was rebuilt on.

**Two variants was one too few and one too many.** `conservative` versus
`default` is a dial with no units. Nobody could say which to pick without
running both, and running both means reading every verdict, which is the cost
the oracle existed to remove. Only one variant ships now.

**Nothing could explain it quickly.** The flow crossed the mission unit, a
durable ask, an attention bound, a checkpoint, a resume hook, a bus subject, a
report router, and a second chain. That was a real defect, and the rebuild
fixed it by deleting a layer rather than the feature: the oracle now mounts on
one in-process seam (`hitlservice.Adjudicator`) instead of riding the bus and
the report router's supervisor offer.

## What the removal got right

**For questions, it was a symptom.** A subagent that raises enough routine
questions to need a filter is usually a subagent whose **intent** is too vague.
The lever that reduces those interruptions is the intent, not a second model
reading the overflow. That is still true, and the shipped doc says so.

**The flow around it did not hold up.** The end-to-end test that decided the
removal found real bugs, none of them in the oracle:

- `mission fire --wait` did not return when a run parked. It blocked until a
  terminal status or the timeout, then tore the unit down — so the
  "fire it and walk away" flow it was built for did not work from a terminal.
- The parked ask was then **swept to its on-timeout verdict** rather than held.
- `approvals list` — a read-shaped command — silently resumed stranded runs as
  a side effect.

The oracle was answering questions inside a flow that was broken underneath it.
Removing the answerer first was the smaller half of the fix.

## What the removal got wrong

It generalised from questions to **all asks**, and the argument does not carry.

A subagent hitting an `approve`-tier tool call is not asking a badly-scoped
question. It is hitting the envelope working exactly as designed. You cannot
fix that by writing a better policy, because the only way to remove the
interruption is to move the call to `allow` — which is precisely the blanket
permission you declined to grant. "Write a tighter envelope" has no answer here;
the envelope is already right.

So the two halves needed different treatment:

| | Reduce it by | Adjudicate it because |
|---|---|---|
| **questions** | writing a narrower intent | the intent sometimes already answers it |
| **gated tool calls** | nothing — the gate is correct | per-call judgment is the only thing that can decide it |

The second row is why the oracle came back.

The removal also left the substrate half-dismantled: the heartbeat sweep's
"widen the bound to the longest open ask" clause became dead code, the report
router's supervisor seam lost its only producer, and the mission preamble told
every subagent that nobody would ever answer it.

## The one that only showed up after

`mission_start` **blocks the parent's turn** while the subagent runs. A parent
that spawned a subagent therefore cannot answer that subagent's question — it is
parked inside its own tool call.

Nothing in the removal accounted for that, because `mission_start` did not exist
yet. With subagents as the prime path, the oracle is not a convenience on top of
the design: it is the answerer for the one case the design structurally cannot
answer itself.
