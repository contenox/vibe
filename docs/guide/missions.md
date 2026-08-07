---
title: Missions
description: Fire a one-line intent at a declared agent under an authored envelope, and read the durable record it leaves behind — states, hosts, reports, questions, reclaim, and compute bounds.
---

# Missions

A mission is a one-line intent fired at a declared agent, run unattended inside an **envelope** — a named HITL policy file that bounds what the unit may do without you. The unit works on its own and reaches you only through its mission tools: a report, a question, or a terminal verdict. Everything it produces is durable, so a mission survives the process that fired it, the terminal you closed, and the machine you rebooted.

This page is the lifecycle: what a mission is, what fires one, what the states mean, how a question gets answered, and what the runtime does when nobody is left to ask.

## Mission, chain run, session

Three things run work in contenox, and they are not interchangeable.

| | What it is | Who drives it | What survives |
|---|---|---|---|
| **Chain run** (`contenox run`) | One stateless execution of a chain file | The invoking process, start to finish | The captured execution state (`contenox state show <reqID>`) |
| **Session** (`contenox new` / `contenox resume`, `contenox acp`, `contenox chat`) | An attended conversation: you prompt, you approve each gated call | You, turn by turn | The session history |
| **Mission** (`mission fire`, `/mission`) | An unattended work order: one intent, one agent, one envelope | The runtime's drive loop, unattended | The mission record, its reports, its plan, and its asks |

A mission dispatches a **unit**: a child subprocess running the agent, with its own session underneath. You do not prompt that session. The runtime does, under the rules below.

## The envelope

Every mission names an envelope at fire time, and a dispatch with no envelope is refused — there is no implicit default inside the runtime. The envelope is an ordinary [HITL policy file](/docs/guide/hitl/) resolved by name along the policy search path (the workspace `.contenox/` first, then `~/.contenox/`, first match wins), and it supplies:

- the per-tool `allow`/`approve`/`deny` rules the unit's tool calls are judged against,
- the `attention` bounds — whether an agent may answer this unit's questions, and how many,
- the `compute` ceilings (see [Compute bounds](#compute-bounds-and-what-actually-enforces-them)),
- the model and backend allowlists the unit resolves within.

Binding the envelope **at fire time** is the point. The name is written into the mission record and shown by `mission list` and `mission show`, so the bounds a unit ran under are a durable fact you can read back later — not a global setting that may have changed since. Two missions fired an hour apart under different envelopes stay distinguishable forever.

```bash
contenox mission fire agent-planner "audit the config loader for unhandled errors" \
  --policy hitl-policy-strict.json --wait
```

Without `--policy`, the envelope comes from the `default-mission-policy` config key. If neither is set, the fire is refused rather than guessing.

## States

A mission has one running state and four terminal ones. A terminal mission never moves again.

| Status | Meaning | Who sets it |
|---|---|---|
| `open` | Dispatched and being driven | The runtime, at creation — every mission starts here |
| `landed` | The work succeeded | The unit, via `mission_finish` |
| `derailed` | It failed and needs a post-mortem | The unit via `mission_finish`, or the runtime when the unit's turn errors |
| `stuck` | A wall, a loop, or a judgement it may not make unattended | The unit via `mission_finish`, or the runtime when a compute bound is crossed |
| `abandoned` | Ended without a verdict of its own | You, via `mission stop`; or the runtime's reclaim sweep when the host is gone |

`stuck` is deliberately not a flavour of `derailed`. A derailed mission asks for a post-mortem; a stuck one asks for a human's attention on a boundary the unit reached honestly.

The terminal transition is guarded and idempotent: finishing a mission twice with the same status is a no-op, and a different terminal status over an already-finished mission is a conflict. Whichever writer got there first owns the outcome.

## `--wait` is required, and why

`contenox mission fire` embeds the fleet **in-process**. The unit it dispatches is a child subprocess of that CLI invocation, so when the command exits, its unit is torn down with it. A detached fire from a one-shot CLI would tear down its own mission, so the flag is mandatory rather than optional:

```bash
contenox mission fire agent-planner "review the open PR for regressions" --wait
contenox mission fire agent-planner "update the README quickstart" --wait --timeout 15m
```

`--wait` polls the durable record until the mission reaches a terminal status. `--timeout` (default `30m`) bounds the wait; on timeout the unit is torn down with the process, and the exit status is non-zero. The mission record and every report filed so far survive either way — `contenox mission show <id>` still reads them.

Exit status is `0` only when the mission **lands**. Derailed, stuck, abandoned, interrupted, and timed-out all exit non-zero.

### Fire-and-detach needs a long-lived host

The unit's lifetime is its host's lifetime. To fire a mission and keep working, fire it from a host that stays up:

| Host | How you fire | Notes |
|---|---|---|
| `contenox acp` (editor session) | `/mission <intent>` | Reports stream back into the firing session as they land |
| `contenox new` (terminal UI) | `/mission <intent>` | Same slash command, same envelope selection |
| `contenox mission fire --wait` | the CLI | Blocking; the unit dies when the command returns |

There is no daemon and no background mission service. This is stated plainly rather than worked around: process supervision is the host's job, and the durable record is what makes a dead host survivable.

`/mission` on its own fires nothing — it prints the grammar, the defaults in force, and every envelope on the search path with its character. See [The `/mission` slash command](/docs/reference/contenox-cli/#the-mission-slash-command).

## The drive loop and the two-turn rule

Nobody is reading the unit's chat. Prose alone reaches no one, so the runtime drives exactly two prompt turns and never a third:

1. **Turn 1** — a preamble telling the unit it is unattended and that its only channels are `mission_report`, `mission_ask_attention`, `mission_plan`, and `mission_finish`, followed by your intent verbatim.
2. If the mission was **reached** — a report filed, a plan revised, or a terminal verdict recorded — the loop stops there.
3. **Turn 2 (the nudge)** — one follow-up telling the unit its last turn reached nobody and naming the tools again.
4. If it is still mute, **the runtime files the blocker itself**: a `blocker` report on the mission quoting the unit's last words and naming the session to attach to.

That last step is the design commitment. A mute unit does not silently disappear into an `open` row; it leaves a durable record saying it went silent and where to look. There is no third prompt, ever.

A turn that **errors** does not go through the nudge path at all: the runtime files a blocker with the error, finishes the mission `derailed`, and stops the unit so it stops holding fleet width. Fleet width is capped by `fleet-max-parallel` (default 8, `0` for unlimited); a dispatch past the cap is refused with a message naming the count and the key.

Liveness is stamped on every completed turn and on every mission tool call — never on a timer, and never on a failed turn. That heartbeat is what the reclaim sweep reads.

## Reports, asks, and the inbox are three different things

These are routinely confused. They are separate mechanisms with separate durability.

| | What it is | Where you read it | How it ends |
|---|---|---|---|
| **Report** | Something the unit chose to tell you: `progress`, `finding`, `blocker`, or `result` | `contenox mission show <id>`, `contenox mission reports <id>` | It doesn't — it is a permanent line in the mission's record |
| **Ask** | A question or a permission gate the unit is *parked on*, waiting | `contenox approvals list`, `contenox mission asks [id]` | You answer it, an agent answers it within bounds, or it expires to its `on_timeout` verdict |
| **Inbox item** | A report that had no live session to deliver to | `contenox inbox list`, `contenox inbox show <id>` | `contenox inbox ack <id>` marks it read without deleting it |

A report *blocks nothing*. An ask *blocks the unit*. The inbox is not a third kind of thing — it is where reports land when the mission has no parent session listening, which is always the case for an operator-fired `mission fire` and eventually the case for any session-fired mission whose session process ended.

A `result` report may carry a structured **hand-over** — outcome, artifacts by reference, a brief for the next mission, and caveats — so a follow-up mission starts from real context. Reports carry references only, never inline artifact content.

`mission show` also surfaces one honesty signal inline: a report the verification gate downgraded because it claimed an artifact that does not exist is printed with a `⚠ claimed artifacts not found` warning beside it.

## How a question gets answered

When a unit calls `mission_ask_attention`, the call **blocks** and a durable ask row is written. The answer comes back to the unit as that tool call's result, so it continues on the same turn.

Three things can resolve it:

- **You answer it.** `contenox approvals respond <ask-id> --answer "use the staging database"`. This is the one verb that answers every pending ask in the system, question or permission gate, mission-bound or not. `contenox mission asks` only narrows the *view* to one mission (or every open one) — it answers nothing.
- **An agent answers it, within the envelope's attention bounds.** If the envelope grants `attention.allowAgentAnswers`, the firing session's agent may answer a bounded number of the unit's routine questions. The budget is durable and actor-aware: a restart does not refill it, and your own answers do not consume it. See [who may answer a unit's question](/docs/guide/hitl/#who-may-answer-a-units-question-attention).
- **It expires.** An unanswered ask resolves to its `on_timeout` verdict; the question is recorded as a blocker instead.

An ask does not hold a process hostage. After a short park window the run **checkpoints** and releases its process; the ask stays a durable row that any later process can answer, and answering it resumes the suspended run exactly once. That is why you can close the terminal that raised a question and answer it tomorrow from a different one.

Under opt-in-beta, `mission fire --oracle` mounts an in-process driver that reviews an operator-fired mission's routine questions and answers them as agent `"oracle"` inside the same attention bounds — everything else still waits for a human. See [The attention oracle](/docs/use-cases/auto-attention/).

## Reclaim: what happens when the host dies

A unit is a child of its host. Close the laptop, kill the terminal, crash the process — the unit dies and its mission row stays `open`, holding a heartbeat that will never advance again. Nothing polls for this; it is collected by a sweep.

**The bound is a floor of six hours of heartbeat silence**, and it is deliberately generous. Reaping live work is unrecoverable; reaping late only delays a row you were already ignoring. The floor is widened further when the mission has a park still open on it: an ask configured to wait longer than six hours explains the silence, so the sweep waits out that ask's own window instead.

The sweep runs when an operator or a host does something that needs the truth:

| Trigger | When it sweeps |
|---|---|
| A host coming up (`fleetservice` build) | Every host boot — a host starting is the moment to collect what a dead one left behind |
| `contenox mission list` | Every run |
| `contenox mission show <id>` | Every run |
| `contenox doctor` | Text output only — `doctor --json` does not sweep, because the JSON payload has no field to report the count in |

A reclaim is never silent. It finishes the mission `abandoned` with the status reason `reclaimed: host process gone` plus the measured silence, files a `blocker` report explaining it, and the commands above print how many they reclaimed. A mission you stopped yourself reads differently (`stopped by operator`), so the two are never confused.

The reclaim collects the **mission record only**. Any run it checkpointed is untouched, and any ask it filed stays pending on its own expiry.

## Compute bounds, and what actually enforces them

An envelope's optional `compute` block declares ceilings. Every field is opt-in and every absent field is unbounded. Enforcement status differs per field, and the runtime is explicit about it rather than implying uniform coverage:

| Field | Status | Detail |
|---|---|---|
| `maxTurns` | Enforced, and only `1` is accepted | The drive loop issues at most two prompt turns, so `1` means "drop the nudge". Any other non-zero value is refused at validation, because it would name a turn the dispatcher was never going to take. |
| `maxTokens` | Enforced between turns, best-effort | Checked against the unit's own reported usage, only between turns — never mid-turn, and inert when the unit reports no usage at all. |
| `maxToolCalls` | **Declared, not enforced** | Validated for shape, and its one enforcement seam is the unattended permission answerer, which no shipped host wires. The envelope picker in `/mission` labels it `declared, not enforced` for exactly this reason. |
| `modelAllowlist` / `backendAllowlist` | Enforced at the resolution seam | Covers chat, prompt, stream, and embed. |
| `onExhausted` | Only `finish_stuck` is implemented | `pause_ask` was declared but never built, and is now rejected at validation rather than silently treated as `finish_stuck`. |

Crossing an enforced bound is never silent: the mission is finished `stuck` with a reason led by `compute bound exhausted`, naming the bound it crossed.

Nothing catches an over-declared bound at author time. `contenox vet` is silent about `maxToolCalls` and `maxTokens` — its `WARN` lines cover only trusted-binary declarations that no longer match this host. What carries the disclosure is the `//compute-fields` note each shipped preset keeps in its own file, and the `declared, not enforced` label in the `/mission` envelope picker.

## End to end

Set the default envelope once, then fire.

```bash
contenox config set default-mission-policy hitl-policy-default.json
contenox agent list                      # what you can fire at
```

Fire a mission and block on it:

```bash
contenox mission fire agent-planner "audit the config loader for unhandled errors" --wait --timeout 20m
```

```
Mission fired at agent "agent-planner" under envelope "hitl-policy-default.json".
Intent: audit the config loader for unhandled errors
Mission 9f3c… (instance 4b1a…, session sess-77c2…).
Waiting for a terminal status (timeout 20m0s; the unit is a child of this process and is torn down when it exits)…
```

While it runs, from **another terminal**, look at what is pending:

```bash
contenox approvals list      # every pending ask, question or permission gate
contenox mission asks        # narrowed to open missions' questions only
```

If the unit raised a question, answer it in your own words. That answer goes back as the tool call's result and the unit continues:

```bash
contenox approvals respond 0f2b… --answer "only the loader in internal/config; skip the legacy path"
```

When it finishes, `fire --wait` prints the verdict and the report summaries. Read the full record any time afterwards:

```bash
contenox mission show 9f3c…       # record, plan summary, pending asks, report summaries
contenox mission reports 9f3c…    # every report in full, oldest first
contenox mission plan 9f3c…       # the living plan and its revision history
```

If the mission was fired by an operator rather than a session, its reports also landed in the durable inbox:

```bash
contenox inbox list
contenox inbox ack <id>
```

And if you need to end one early:

```bash
contenox mission stop 9f3c… --reason "requirements changed"
```

`stop` abandons the mission, closes its pending asks, and reaps the unit wherever it is running — the terminal-status event reaches the live host over the shared bus.

## Limits, stated plainly

- **No detached fire from the CLI.** `--wait` is mandatory. Fire-and-detach requires a long-lived host.
- **No daemon.** Nothing runs missions in the background on your behalf.
- **Two turns, never three.** A unit that goes mute gets one nudge and then a runtime-filed blocker.
- **Reclaim is lazy, not scheduled.** With no host booting and no operator running `mission list`, `mission show`, or `doctor`, a dead host's mission stays `open` until something looks.
- **`maxToolCalls` does not bound anything today.** Rely on the per-tool `deny` rules in the envelope instead.

## Next

- [HITL policies](/docs/guide/hitl/) — the envelope format, the presets, and the `attention` bounds
- [Troubleshooting](/docs/guide/troubleshooting/) — a mission stuck in `open`, recovering after a crash, and `doctor --bundle`
- [The attention oracle (beta)](/docs/use-cases/auto-attention/) — `mission fire --oracle`
- [Authored approval](/docs/use-cases/authored-approval/) — the envelope as a reviewable artifact
- [Events & triggers (beta)](/docs/guide/events/) — reacting to mission reports, status changes, and asks
- [`contenox mission` reference](/docs/reference/contenox-cli/#contenox-mission) — every flag
