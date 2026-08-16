---
title: Events & triggers (beta)
description: The durable local event log, operator-authored trigger files that fire chains from it, and the exact guarantees the dispatcher makes — and does not make.
order: 12
---

# Events & triggers (beta)

Contenox's internal domain events — mission reports, status changes, plan revisions, attention asks — land in a durable, append-only log inside the local database. Operator-authored `trigger-*.json` files bind an event type to a task chain, and firing happens on two paths that reconcile through one durable record:

- **Live, in-process.** A host that runs an engine (`contenox acp`, or `contenox mission fire` when it builds one) fires matching triggers the moment it appends an event — same process, same engine, no extra daemon.
- **Catch-up.** `contenox events dispatch` reads the log from a durable cursor and fires whatever was appended while no engine-running host was up. It is a foreground process, not a daemon: you keep it alive with the tools you already trust — tmux, systemd, `nohup` — and while nothing is running, events simply wait in the log.

Both paths claim each firing in the same durable table before running it, so a (trigger, event) pair fires at most once no matter which path saw it first. That is the Unix stance, held on purpose: process supervision is a solved problem, and the durable cursor means stopping and starting loses nothing.

> **Beta:** the event tier requires `contenox config set opt-in-beta true` (or `CONTENOX_OPT_IN_BETA=1`) and its interface may change. Without the opt-in, `contenox events` is hidden from help and no trigger file loads.

## The event log

The log lives in the same local SQLite database as everything else. Its mechanics are the foundation the guarantees below rest on:

- **Append-only, one table per UTC day.** Retention is dropping whole days with [`events prune`](#retention-events-prune) — never automatic.
- **A global sequence number.** Every event gets an `nid` from one monotonic sequence, so append order is total across days. The dispatcher's durable cursor is an nid.
- **An acceptance window.** An append is accepted only when the event's time is within ±10 minutes of now, which keeps a day's events in that day's table. Internal producers stamp "now", so this bound is a schema guard, not something you manage.
- **Dual-write, store first.** Producing services append to the log durably, then publish a live in-process copy. The live copy is only a wake-up call for the dispatcher; the log is the record.
- **Workspace-scoped.** Every event carries the workspace it happened in, and every read filters by it.

## The event shape

Every stored event is one JSON envelope — and this exact envelope is what a fired chain receives as its input:

```json
{
  "nid": 42,
  "workspace_id": "2f6c1c4e-…",
  "type": "missionservice.events.report_added",
  "source": "missionservice",
  "subject": "mission-01H…",
  "time": "2026-08-04T09:15:00Z",
  "data": { "missionId": "mission-01H…", "report": { "…": "…" } },
  "hop": 0
}
```

| Field | Meaning |
|---|---|
| `nid` | Global sequence number — monotonic across all days; the cursor and dedup key |
| `workspace_id` | The workspace the event happened in; triggers only ever see their own workspace's events |
| `type` | What happened — the exact string a trigger's `listen_for.type` must match |
| `source` | The producing service (`missionservice` for every V1 event) |
| `subject` | The entity concerned — for mission events, the mission id |
| `time` | Event time, UTC |
| `data` | The producer's JSON payload, verbatim |
| `hop` | Dispatch-generation counter: `0` for ordinary operation; an event appended by a chain the dispatcher fired carries its causing event's hop + 1; past hop 4 nothing fires (the [loop guard](#guarantees)) |

## What emits events: the V1 catalog

V1 emits exactly four event types, all from the mission tier:

| Type | Appended when | `data` carries |
|---|---|---|
| `missionservice.events.report_added` | A mission unit files a report | The mission id, the full report, and routing fields (parent session, agent name, intent) |
| `missionservice.events.status_changed` | A mission reaches a terminal status | Old and new status, the reason, and the same routing fields |
| `missionservice.events.plan_revised` | A mission's living plan is revised | The revision number, an explanation, and entry counts (added/removed/pending) |
| `missionservice.events.attention_asked` | A unit raises a question for a human | The durable ask's id (answerable via `contenox approvals respond`), a summary, and detail |

These are internal events only: the log records what this runtime did. Contenox is not an external event sink — no endpoint or command accepts events from outside.

## Authoring a trigger

A trigger is a `trigger-*.json` file:

```json
{
  "name": "on-report",
  "description": "summarise every mission report",
  "listen_for": { "type": "missionservice.events.report_added" },
  "type": "fire_chain",
  "chain": "chain-on-report.json",
  "policy": "hitl-policy-default.json"
}
```

| Field | Meaning |
|---|---|
| `name` | Unique trigger name — the firing records key on it |
| `description` | Optional, for humans |
| `listen_for.type` | The exact event type to react to — exact string match, no globs or prefixes |
| `type` | Always `"fire_chain"`; the only trigger action |
| `chain` | The chain file to run, as a file name (never a path), with the event envelope as its JSON input |
| `policy` | Optional envelope for the fired run — a [HITL policy](/docs/guide/hitl/) file name; omitted, the standard policy resolution applies |

A trigger grants timing, never capability — the fired chain runs under an operator-authored envelope like any other run. What the chain may do is decided by its tool allowlist and its policy, exactly as if you had started it by hand; the trigger only decides *when* it starts.

`chain` and `policy` are resolved by name, workspace-first: the workspace `.contenox/` copy wins, `~/.contenox/` is the fallback, and a same-named workspace file shadows the home copy. Trigger files themselves are discovered the same way. See [Chain files: naming, roles, and resolution](/docs/guide/chains/naming/) for the full resolution story.

`contenox vet` validates trigger files — the shape above, plus that the named chain and policy actually resolve on the search path. At dispatch start, a malformed trigger file is skipped with a printed warning and never fires; `contenox doctor` lists what loaded and what was skipped.

## In-process firing: the live path

No command turns the live path on — it is part of running a host. When an engine-running host (`contenox acp`, or a `contenox mission fire` that built an engine for loaded triggers) appends an event under opt-in-beta, it fires matching triggers immediately, in its own process, on its own engine. The firing is asynchronous to the append: a chain failure is recorded on the firing record and never fails or delays the event's append. A host that stops mid-firing leaves that firing claimed, exactly like a dispatcher crash would — and the same stale-claim takeover (see [inspecting firings](#inspecting-firings-events-firings)) recovers both.

Events appended by a process that runs no engine (`contenox mission stop`, a bare read verb) fire nothing live; they wait in the log for the catch-up dispatcher.

## Running the catch-up dispatcher

```bash
contenox events dispatch
```

The dispatcher prints its loaded triggers, catches up on every event appended since the last dispatcher stopped, then follows new events live. One line is printed per firing — trigger, event type, nid, hop, status, request id. Stop it with Ctrl-C. With live in-process firing wired into the hosts, the dispatcher's duty is catch-up: events appended while nothing ran, and hosts that run no engine. The shared firings table dedups the overlap — an event both paths saw fires once.

There is deliberately no daemon. Run it under whatever supervision you already use:

```bash
tmux new -d -s contenox-events 'contenox events dispatch --auto'
# or a systemd unit, or nohup contenox events dispatch --auto &
```

Fired chains run under an envelope like any run: without `--auto`, approve-tier tool calls surface in the dispatcher's terminal when it is attended, and otherwise park as durable asks (`contenox approvals list`). `--auto` disables the terminal prompts for unattended operation — the trigger's policy (or the default) still applies.

| Flag | Description |
|---|---|
| `--auto` | Non-interactive mode: no terminal approval prompts; fired chains route through the trigger's policy (or the default) without a terminal ask |

### Inspecting the log: `events list`

```bash
contenox events list               # from the start of the log, in append order
contenox events list --since 41    # events with nid > 41
```

Lists the current workspace's events in nid order: nid, type, source, subject, hop, time, and a compacted payload. `--limit` caps the page (default 50).

### Inspecting firings: `events firings`

```bash
contenox events firings                       # the most recent firings, newest first
contenox events firings --status error        # chains that failed
contenox events firings --status refused      # hop-limit refusals
contenox events firings --trigger on-report   # one trigger's history
contenox events firings --since 41            # firings for events with nid > 41
```

`events list` shows what was **appended**. `events firings` shows what was **dispatched** — the durable claim record both firing paths write, so the dispatcher's work is visible when it works and not only when it crashes. One row per (trigger, event) claim: nid, trigger, status, the `evt-` request id, the outcome time, and the recorded error.

```
NID  TRIGGER    STATUS   REQUEST               TIME                  ERROR
121  on-report  refused  evt-1f77ab30c95e2d04  2026-08-04T20:23:43Z  eventtrigger: event 121 hop 5 exceeds limit 4; refusing to fire "on-report"
119  on-status  error    evt-9d02c7a41fb6e830  2026-08-04T20:23:43Z  task "summarize": model resolve failed: no backend for qwen3:8b
118  on-report  ok       evt-4b1c9a02f7e3d551  2026-08-04T20:23:43Z
```

`ok`, `error`, and `refused` are outcomes; `running` is a claim with no outcome yet — a chain still executing, or one whose host died before it could record how things ended. A dead host's claim does not hold the pair forever: a `running` row untouched for two hours is stale, and the next claim attempt for that (trigger, event) pair takes it over and fires again — a crash costs a wait, not the firing. The takeover is a recovery path, not a schedule: nothing walks the table hunting stale claims, so a stranded firing that no path ever re-claims stays `running`, which is exactly what `contenox doctor` counts (below). The listing is workspace-scoped like every read of the log, and `--limit` caps the page (default 50, ceiling 1000). No match prints `(no firings)` and exits 0 — an empty answer, not a failure.

> **Note:** the typo case leaves no error row to find. A trigger whose `listen_for.type` matches nothing records *nothing at all*: `contenox doctor` still lists it as loaded, and `events firings --trigger <name>` comes back empty. Compare the trigger's type against the types in `contenox events list` — a type that never appears there is the bug.

`contenox doctor` adds one line under its beta section when the recent window went wrong (`Event firings: 2 of the last 50 ended in error/refused or are stranded mid-run — contenox events firings`) and stays silent when it did not.

### Retention: `events prune`

```bash
contenox events prune --keep-days 30
```

Drops whole per-day partitions older than the window — one O(1) table drop per day, no row deletes, no VACUUM. Pruning runs only when you invoke it, never automatically, and asks for confirmation unless `--yes` is passed. The dispatch cursor and the firing records are untouched.

## The observability plane reads the event plane; it never writes to it

**Hard invariant: firing observability is a read. Nothing that observes firings may append an event.**

`Append` fires matching triggers in-process. A telemetry sink that publishes — the convenient "record each firing as an event" — therefore closes the loop: a firing appends an event, which fires triggers, which append events. The amplification is unbounded, and it peaks exactly during an incident, when firings are already spiking and the record is the one thing that has to stay readable.

Firing observability rides two things that cannot produce events:

- **The durable `event_firings` rows**, read by `contenox events firings` and by `contenox doctor`. The store's listing method issues a `SELECT` and nothing else.
- **`libtracker`**, the one instrumentation seam. Failed and refused firings are reported through the tracker, which writes to its configured sink.

There is no published firing feed, and there must never be one.

> **Note:** this is why packages report through `libtracker` rather than calling `log/slog` as an API. `slog.Default()` is global and reconfigurable at runtime, so a rule about what logging may do cannot be enforced against it — any package can swap in a handler that publishes. Routing every report through one seam makes the never-produce rule checkable in one place; a repo guard (`libtracker/slog_guard_test.go`) fails the build on any `log/slog` import outside the sink adapter and a named allowlist of composition roots.

## Guarantees

The dispatch tier guarantees:

- **At most one live firing per (trigger, event) — across restarts, across the in-process/catch-up split, and across the live/catch-up overlap inside the dispatcher.** Every firing, on every path, is claimed in one durable table before the chain runs, and the claim is what dedups. A crash mid-chain (host or dispatcher) leaves that firing claimed but unfinished; a claim untouched for two hours is stale, and the next claim attempt for the pair takes it over and fires again. The bound sits above the longest run a live firing can legitimately reach, so a slow but living chain is not stolen and run twice. The flip side is stated too: a retried chain starts from the beginning, and whatever the dead attempt already did is not undone — the price of not losing the firing.
- **Firings happen live, in-process, while an engine-running host is up.** An event such a host appends fires its triggers immediately in that process; the standalone dispatcher exists for everything appended while nothing ran.
- **The cursor advances only after an event is handled.** A killed dispatcher resumes at exactly the event it stopped on. (The in-process path keeps no cursor — it is live-only; the cursor belongs to catch-up.)
- **Events appended while no engine-running host and no dispatcher ran are fired on the next dispatcher start.** The durable log is the truth; the live bus is only a nudge. A missed live delivery delays a firing, it never loses one.
- **One workspace's triggers never fire on another workspace's events.** Both paths process only their own workspace's events; the dispatcher's cursor is per-workspace.
- **A failing chain never stops the loop.** The failure is recorded on that firing (`status=error`) and dispatch continues; on the in-process path the append that caused the firing has already succeeded.
- **Trigger loops die out.** A fired chain stamps hop + 1 on every event it appends; an event past hop 4 is refused (`status=refused`), never fired — on both paths.

It does not guarantee:

- **Firing latency on the catch-up path.** Live deliveries only shorten the dispatcher's wait; the backstop is a poll. Only an in-process firing is immediate, and only while its host runs.
- **Identical tool posture across paths.** An in-process firing runs on its host's engine with that host's tool registrations; the standalone dispatcher ships with `local_shell` on. A chain needing a tool its firing host does not register records an error for that firing (claimed, not retried).
- **Delivery to external systems.** There is none. The log is local, and nothing pushes events anywhere.
- **Retention.** Nothing is pruned until you run `events prune`; until then the log grows.
- **Dispatch with nothing running.** No daemon exists. While no engine-running host and no `events dispatch` process runs, nothing fires — events wait in the log.

## Fired runs are ordinary runs

Each firing executes its chain the same way any other chain run does, under a request id prefixed `evt-` (printed on the dispatcher's firing line and in [`events firings`](#inspecting-firings-events-firings)). `contenox state list` shows each firing's execution and `contenox state show <reqID>` the per-task steps — the same inspection you have for any run.

## Next

- [Event-driven chains: three stories](/docs/rnd/event-driven-chains/) — the trigger tier in use: a phone buzz on `attention_asked`, a completion summary on `status_changed`, and the firing record as an audit trail
- [The oracle](/docs/use-cases/auto-attention/) — an in-process adjudicator rules on a subagent's routine asks (no trigger, no dispatcher involved)
- [HITL policies](/docs/guide/hitl/) — the envelopes fired chains run under
- [Chain files: naming, roles, and resolution](/docs/guide/chains/naming/) — how the referenced files resolve
- [`contenox events` reference](/docs/reference/contenox-cli/#contenox-events) — every flag
