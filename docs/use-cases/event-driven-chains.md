---
title: "Event-driven chains: three stories"
description: "Three working shapes for the beta event tier: a phone buzz when a mission asks for a human, a completion summary written when a mission ends, and the firing record that makes both inspectable."
---

# Event-driven chains: three stories

Missions already leave a durable trail: every report, status change, plan revision, and question lands as an event in the local log. The trigger tier — `trigger-*.json` files an operator writes — turns that trail into reactions: an event of the type you named fires the chain you named, under the [HITL policy](/docs/guide/hitl/) you named. This page is three shapes that mechanism supports today, each built only from what ships.

> **Beta:** the event tier requires `contenox config set opt-in-beta true` (or `CONTENOX_OPT_IN_BETA=1`) and its interface may change. Without the opt-in, `contenox events` is hidden and no trigger file loads. No trigger ships either — `contenox init` seeds none, so every trigger below is a file you write yourself.

All three stories share one setup: the trigger file, its chain, and its policy sit in the workspace `.contenox/` (or `~/.contenox/` — [same resolution as every system file](/docs/guide/chain-naming/#resolution-which-file-wins)), `contenox vet --all` confirms they parse and resolve, and firing happens on the two documented paths — live inside any engine-running host, and by `contenox events dispatch` catching up on everything appended while no such host ran. The [events guide](/docs/guide/events/) is the full contract; this page is what it feels like in use.

## Story 1 — the attention buzz

A mission unit that hits a question it must not decide alone parks on a durable ask. If you are at the terminal, you see it. If you are making coffee, the ask waits silently — correct, and easy to miss for an hour.

The event tier closes that gap. Every ask also appends a `missionservice.events.attention_asked` event, and its `data` carries the ask's id (`askId`), a `summary`, and `detail`. A trigger can hand that event to a chain that posts one line to an [ntfy](https://ntfy.sh) topic your phone subscribes to. Outbound calls like this aren't a built-in toolset anymore — register the endpoint once as a narrow [remote tool](/docs/integrations/tools/remote/) (`contenox tools add ntfy --url https://ntfy.sh --spec ./ntfy-post.yaml`, a one-operation spec), and ntfy itself is nothing more than an HTTP endpoint that turns a POST into a push notification. Your phone buzzes because your own chain called the one tool you registered for it; contenox knows nothing about phones.

The trigger, `trigger-attention-buzz.json`:

```json
{
  "name": "attention-buzz",
  "description": "push a phone notification when a mission unit asks for a human",
  "listen_for": { "type": "missionservice.events.attention_asked" },
  "type": "fire_chain",
  "chain": "chain-attention-buzz.json",
  "policy": "hitl-policy-notify.json"
}
```

The chain, `chain-attention-buzz.json` — one model turn that composes the line and calls the tool, one task that executes the call:

```json
{
  "id": "attention-buzz",
  "tasks": [
    {
      "id": "compose",
      "handler": "chat_completion",
      "system_instruction": "The input is a contenox event envelope. data.summary is a mission unit's question for a human; data.askId answers it. Call ntfy.postMessage exactly once: topic my-fleet-attention, body is one line — the summary, then: answer with contenox approvals respond <askId> --answer '...'",
      "execute_config": {
        "model": "{{var:model}}",
        "provider": "{{var:provider}}",
        "tools": ["ntfy"]
      },
      "transition": {
        "branches": [
          { "operator": "equals", "when": "tool_call", "goto": "post" },
          { "operator": "default", "goto": "end" }
        ]
      }
    },
    {
      "id": "post",
      "handler": "execute_tool_calls",
      "transition": {
        "branches": [{ "operator": "default", "goto": "end" }]
      }
    }
  ]
}
```

The envelope, `hitl-policy-notify.json`, is where the story earns its keep. A trigger grants timing, never capability — so a fired chain whose policy has no `allow` rule for `ntfy.postMessage` would fall through to the default policy's `default_action` (`approve`) and park on an approval *to send the notification that tells you something is parked*. The notify envelope allows exactly one thing:

```json
{
  "default_action": "deny",
  "rules": [
    {
      "tools": "ntfy",
      "tool": "postMessage",
      "action": "allow"
    }
  ]
}
```

One tool, one operation, everything else denied — and because `ntfy` was registered with `--url https://ntfy.sh` in the first place, there's no other host it could reach even if the chain wanted one. The chain cannot read a file, run a command, or call anywhere but the endpoint you registered.

Now fire a mission and walk away. The `mission fire` host runs an engine, so under opt-in-beta it fires the trigger live, in-process, the moment the ask's event is appended; `contenox events dispatch --auto` in a tmux pane is the catch-up for anything appended while no engine-running host was up, and the shared firings table makes sure the overlap fires once. The phone buzzes with the unit's actual question and the command that answers it.

The buzz is one-way, and that is the design: contenox accepts no inbound events, so nothing on the phone talks back through ntfy. You answer where answers live — any terminal:

```bash
contenox approvals respond <askId> --answer "use the staging database"
```

or, on a [machine paired with a relay](/docs/guide/pairing/), from the app.

## Story 2 — the shift report

A mission that ends while you are elsewhere reaches one of four terminal statuses — `landed`, `derailed`, `stuck`, `abandoned` — and appends a `missionservice.events.status_changed` event whose `data` carries `missionId`, `intent`, `oldStatus`, `newStatus`, and the `reason`. A second trigger turns that into a file in the repo: the shift report you read the next morning.

`trigger-shift-report.json`:

```json
{
  "name": "shift-report",
  "description": "write a completion summary when a mission reaches a terminal status",
  "listen_for": { "type": "missionservice.events.status_changed" },
  "type": "fire_chain",
  "chain": "chain-shift-report.json",
  "policy": "hitl-policy-shift-report.json"
}
```

`chain-shift-report.json` has the same two-task shape as the buzz chain, with `local_fs` in place of `ntfy`: the model turn reads the envelope and calls `local_fs.write_file` with a summary of what the mission was, how it ended, and why — one new markdown file per mission, `path` pinned by instruction to `reports/<missionId>.md`.

Instruction is not enforcement, so the envelope repeats the boundary in a form that cannot be talked around:

```json
{
  "default_action": "approve",
  "rules": [
    {
      "tools": "local_fs",
      "tool": "write_file",
      "action": "allow",
      "when": [{ "key": "path", "op": "glob", "value": "reports/**" }]
    }
  ]
}
```

One write path runs unattended. Anything else the chain attempts — a write elsewhere, a shell command, a network call — matches no rule and fail-closes to approval: under `dispatch --auto` there is no terminal prompt, so it parks as a durable ask in `contenox approvals list` instead of running. `--auto` removes the prompt, never the envelope.

Paths resolve against the dispatcher's working directory, so start `contenox events dispatch --auto` from the repository root and the summaries land in `reports/`. Missions that finished overnight are a `ls reports/` away — and because the fired chain is an ordinary run, each summary's full execution is still in `contenox state list` if you want to see how it was written.

## Story 3 — the audit trail

The first two stories only pay off if you can trust them while not watching — and trust here is a read, not a feeling. Two commands split the question "did my automation work?" into its two honest halves:

```bash
contenox events list       # what happened: every event, in append order
contenox events firings    # what was dispatched: every (trigger, event) claim and its outcome
```

`events list` is the producers' record — mission events with their `nid`, type, subject, and payload, whether or not any trigger cared. `events firings` is the dispatch record, one row per claim, written before each chain runs on either firing path:

```
NID  TRIGGER         STATUS   REQUEST               TIME                  ERROR
214  shift-report    ok       evt-4b1c9a02f7e3d551  2026-08-10T21:40:12Z
213  attention-buzz  error    evt-9d02c7a41fb6e830  2026-08-10T21:39:58Z  task "compose": model resolve failed: no backend for qwen3:8b
209  attention-buzz  ok       evt-1f77ab30c95e2d04  2026-08-10T18:02:31Z
```

The statuses carry the operational story:

- **`ok`** — the chain ran. The `evt-` request id is a handle: `contenox state show evt-9d02…` replays the firing task by task, exactly like any run.
- **`error`** — the chain failed, the failure is on the record, and dispatch went on. A failing chain never stops the loop; `contenox events firings --status error` is the morning sweep.
- **`refused`** — the hop guard held: an event past hop 4 (a chain fired by a chain fired by a chain…) is refused rather than fired, so a trigger loop dies out instead of melting the log.
- **`running`** — a claim with no outcome yet: a chain still executing, or one whose host died mid-firing. A dead host's claim goes stale after two hours and the next claim attempt takes the pair over and fires again — but nothing walks the table hunting strays, so a firing no path ever re-claims stays `running`. That is what "stranded" means, and `contenox doctor` counts it: one line under its beta section when the recent window holds errors, refusals, or stranded runs, silence when it does not.

One failure mode leaves no row at all: a trigger whose `listen_for.type` matches nothing — a typo, since matching is exact — records nothing, fires nothing, and still shows as loaded. `events firings --trigger <name>` coming back empty while `events list` shows the events you meant is that bug's signature; compare the strings.

Two properties keep the record trustworthy. Observability never feeds back: reading firings appends nothing, so inspecting an incident cannot amplify it. And retention is yours: the log grows until *you* run `contenox events prune --keep-days 30` — nothing is dropped behind your back.

## The boundary

The same honest edge closes all three stories. Triggers are **beta** — behind `opt-in-beta`, interface subject to change. They are **operator-authored** — nothing ships enabled, nothing fires until you write a file and can read back every word of what it may do. And they are **local** — the only producers are this runtime's own mission events (the four types above), there is no inbound endpoint, no schedule, no external webhook source, and no delivery guarantee to anywhere: the ntfy buzz is your chain calling an endpoint your envelope allowed, not a channel contenox maintains. While no engine-running host and no dispatcher runs, nothing fires — events wait in the log, and the next `contenox events dispatch` picks up exactly where the cursor stopped.

## Next

- [Events & triggers (beta)](/docs/guide/events/) — the event shape, both firing paths, and every guarantee
- [`contenox events` reference](/docs/reference/contenox-cli/#contenox-events) — every flag
- [HITL policies](/docs/guide/hitl/) — the envelopes fired chains run under
- [Missions](/docs/guide/missions/) — the tier that produces every event above
- [Pairing a machine with a relay](/docs/guide/pairing/) — answering from the app instead of the desk
