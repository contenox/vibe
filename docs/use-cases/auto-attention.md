---
title: The attention oracle (beta)
description: mission fire --oracle mounts an in-process driver that answers routine mission questions so unattended runs finish; consequential asks still wait for you.
---

# The attention oracle (beta)

Unattended missions stall on routine questions. You fire a mission, walk away, and come back to a run parked on an ask its own intent already answers — "which directory holds the docs?" on a mission whose intent names the docs — instead of coming back to done. The **oracle attention driver** keeps the shift moving: mounted with one flag on the fire itself, it reviews each question the mission raises, answers the ROUTINE ones so the run continues, and leaves everything else exactly where it was — a durable ask waiting for a human. WAIT is always the safe verdict, and the durable record shows exactly who answered what.

> **Beta:** the driver requires `contenox config set opt-in-beta true`; without the opt-in the `--oracle` flag does not exist (absent from help, refused as unknown) and the `--as-agent` respond flag does not exist either.

## Activation: one flag, no dispatcher

```bash
contenox mission fire agent-planner "update the README quickstart" \
  --wait --policy hitl-policy-default.json --oracle
```

That is the whole activation. No trigger file, no event dispatcher, no second process: `--oracle` makes the `mission fire` host build an engine and mount the driver in-process. When the command exits, the driver is gone with it — the mode is exactly as alive as the fire that mounted it.

## The driver model

The driver is a supervisor mounted on the **same seam the agent-to-agent answer path uses** (the report router's `AgentSupervisor` offer). When an editor session fires a mission, a unit's question is offered to the agent driving that session; when an operator fires a mission from the CLI, there is no such agent — `--oracle` mounts the oracle as that seam's sibling. The two self-select and never overlap: the firing-agent offer takes only questions with a parent session, the oracle takes only operator-fired ones.

What happens on a question, step by step:

1. The unit calls its `mission_ask_attention` tool. The ask becomes a durable row immediately (`contenox approvals list` shows it), and the unit parks on it for a short window (30s) before checkpointing.
2. The host's report router offers the ask to the driver. The driver runs the **oracle chain** (`chain-oracle-default.json`) in-process on the host's engine, with the ask event as input.
3. The chain's model holds one tool, `oracle.submit_verdict`, and one job: judge routine-or-not, then submit `{"verdict":"wait"}` or `{"verdict":"answer","answer":"<one short plain sentence>","askId":...}`. The loop is budgeted and self-correcting — a malformed call or a chat-text reply gets a machine-register correction and a bounded retry; details below.
4. A valid ANSWER is delivered through the **service layer**: the same bounds enforcement `approvals respond --as-agent` runs (`attention.allowAgentAnswers` / `maxAgentAnswers` on the *mission's* envelope, counted on durable records), then the same `AnswerAsAgentNamed` delivery an a2a agent answer takes, recorded as `answeredBy: "oracle"`. The parked unit's poll picks the resolved row up and the run continues — in the window, with **no checkpoint ever created**.
5. On WAIT, a bounds refusal, a malformed contract that exhausted its budgets, or a chain error: the driver does **nothing**. The untouched normal path proceeds — park, checkpoint, durable ask, human. A WAIT changes nothing at all: the ask stays pending until you answer it or it expires.

If the oracle needs longer than the park window, the ask checkpoints normally; the driver's late answer then goes through the existing durable respond path, which resumes the suspended run — the same shape a late human answer has.

## The verdict contract

The oracle chain is the shipped agentic loop (chain-agent-run's shape: chat → tool_call → execute → back, with a recovery loop and edge budgets) stripped to one tool. Every failed attempt produces an observable result the model can correct from, within small budgets:

- a malformed `submit_verdict` payload (bad verdict value, missing answer, wrong `askId`) comes back as a corrective tool result naming exactly what was wrong — wrong `askId` names the expected source field (the INPUT event's `askId`);
- a chat-text reply instead of a tool call routes through a deterministic gate that feeds back a machine-register correction ("output rejected: submit the verdict via submit_verdict") and retries;
- a transient delivery error surfaces as the tool result and may be retried;
- a **bounds refusal is final**: the envelope holding comes back as the tool result, the contract ends WAIT-equivalent, and it is never retried.

Budget exhaustion ends the chain with no verdict — WAIT-equivalent, nothing executed.

Two seeded variants carry the same contract: `chain-oracle-default.json` (balanced: intent plus the ask's own context may imply the answer) and `chain-oracle-conservative.json` (strictest WAIT bias: the intent alone must resolve it). The driver currently runs the default variant; a config knob for selecting the conservative one is a follow-up.

## No shell, nothing to execute

The oracle executes nothing. Its envelope, `hitl-policy-oracle.json`, is `default_action: deny` with exactly two allows — `oracle.submit_verdict` (the verdict tool) and `oracle.verdict_state` (the deterministic corrective gate) — and **no shell, filesystem, or network rule of any kind**. There is no command allowlist to subvert: answers travel in-process through the service layer, never through an argv. The `submit_verdict` tool itself exists only inside the driver's own chain execution — it is not in the global tool registry, and no other chain ever sees it.

The oracle's `attention` block is deliberately human-only: the oracle never adjudicates its own asks.

## The mission's envelope must grant it

The oracle's own policy only bounds what the oracle chain may do. Whether an agent may answer a given mission's questions at all is decided by that **mission's** envelope — the HITL policy named when the mission was fired:

```json
"attention": {
  "allowAgentAnswers": true,
  "maxAgentAnswers": 3
}
```

- `allowAgentAnswers` — omitted or `false` means human-only; every oracle answer (and every `--as-agent` respond) is refused.
- `maxAgentAnswers` — how many of the mission's questions agents may answer before the rest wait for a human. Omitted uses the default cap of 3; `0` is not unlimited. The count is durable — counted from the recorded answers, so a restart does not refill it.

The shipped `hitl-policy-default.json` grants exactly this block; `hitl-policy-strict.json`, `hitl-policy-acpx.json`, and `hitl-policy-oracle.json` itself refuse. Enforcement lives in the service layer (one function), so the CLI's `--as-agent` respond, the a2a firing-agent answer, and the oracle driver hold the identical line.

## What the record shows

- **Who answered what.** An oracle-answered ask stores `answeredBy: "oracle"` in the durable resolution; a human answer stores no actor. The attribution survives restarts — it is the row, not a log line.
- **Bounds enforced.** A refusal reads `agent answer refused: envelope ... does not allow agent answers (no attention.allowAgentAnswers grant); a human must answer`, or `agent answer refused: mission ... spent its agent-answer bound (N of N); this question waits for a human`. A refusal is the envelope holding, not an error — the driver's contract ends WAIT-equivalent and the ask keeps waiting.
- **WAIT changes nothing.** The ask stays pending in `contenox approvals list` until a human answers or it expires to its on-timeout verdict.
- **Resume is unchanged.** An oracle answer wakes the parked unit through the same path a human answer uses; the driver prints one trace line per review (reviewing / answered / WAIT / refused / no verdict) to the firing command's stderr.

## Run it

```bash
# 1. Opt in (durable config).
contenox config set opt-in-beta true

# 2. The chains and policy are already seeded by `contenox init`
#    (chain-oracle-default.json, chain-oracle-conservative.json,
#    hitl-policy-oracle.json). Check them:
contenox vet --all

# 3. Fire with the driver mounted — one command, nothing else running:
contenox mission fire agent-planner "..." --wait --policy hitl-policy-default.json --oracle
```

Without `--oracle`, the same fire behaves exactly as before: every question waits for a human (`contenox approvals respond <id> --answer "..."`).
