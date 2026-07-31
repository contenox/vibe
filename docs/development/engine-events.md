# Engine event contract

Status: normative. This document is THE contract between the task engine's
event stream and its consumers — the ACP translators today, the terminal UI's
engine-bridge next, and the checkpoint machinery. The matrix below is
asserted in CI:

- `internal/kernel/taskengine/events_contract_test.go` drives representative
  runs and asserts sequence, fields, and addresses;
- `internal/kernel/taskengine/events_journal_test.go`
  (`TestUnit_KVJournalTaskEventSink_JournalingMatrix`) asserts the journaling
  column;
- `internal/surfaces/acpsvc/events_test.go`
  (`engineEventTranslationMatrix`) asserts the ACP translation column.

Every kind the engine can emit is enumerated by
`taskengine.AllTaskEventKinds()`; adding a kind without updating this document
and those tests fails CI. When code and this document disagree, fix the
drift — never loosen the assertion.

## 1. The event type

Events are `taskengine.TaskEvent`, JSON-marshalled on the wire. Evolution
policy: **additive only** — new fields use `omitempty`/`omitzero`, existing
field names and meanings never change (journal rows and bus subscribers parse
old payloads forever).

Fields present on (almost) every event:

| Field | Meaning |
|---|---|
| `kind` | one of the kinds below |
| `timestamp` | UTC emission time |
| `request_id` | run correlation ID, when the run context carries one |
| `scope` | hierarchical address, see §2 |
| `chain_id`, `task_id`, `task_handler` | flat mirrors of the address (legacy wire shape; stay populated identically) |
| `retry` | task attempt index, 0-based; 0 for chain-level events |

## 2. Hierarchical addresses (`scope`)

`EventScope{chain, task, tool_call}` is the structured address of an event —
the same address `CapturedStateUnit.Scope` carries and the address
checkpoints name resume positions with. Population invariants:

- **`scope.chain`** is set on *every* event emitted during an `ExecEnv` run
  (the executor installs a chain-level scope on the run context before the
  first event).
- **`scope.task`** is set on every event emitted inside a task attempt:
  `step_started`, `step_chunk`, `step_stream_end`, `step_completed`,
  `step_failed`, `token_usage`, `tool_call_pending`, `tool_call`, and any
  approval/HITL event raised while a tool executes. Chain-level events
  (`chain_started`, `chain_completed`, `chain_failed`) and `print` carry no
  task address.
- **`scope.tool_call`** is set exactly on events attributable to ONE tool
  invocation: `tool_call_pending`/`tool_call` (all paths, including the
  error-result path for unresolvable calls), and approval/HITL events emitted
  from within a call context. The value is the engine-minted call ID
  (`ensureUniqueToolCallID`) — unique per invocation even when a model reuses
  IDs across turns. For `tools`-handler tasks it is a synthetic per-invocation
  ID derived from the task ID.

**What checkpointing can rely on:** `(scope.chain, scope.task, retry)`
uniquely names a task attempt within a run; `(scope.chain, scope.task,
scope.tool_call)` uniquely names one tool invocation; `tool_call.approval_id`
equals `scope.tool_call` on the execute_tool_calls paths, so a checkpoint
keyed by approval ID can be joined back to its emitting position without
string parsing. `CapturedStateUnit.Scope` carries `{chain, task}` for every
recorded step.

## 3. Per-kind matrix

"Journaled" = persisted by `KVJournalTaskEventSink` (see §5).
"ACP" = number of notifications the acpsvc translators render for a
representative event (see §6).

| Kind | Fires when | Populated fields (beyond §1) | Ordering | Journaled | ACP |
|---|---|---|---|---|---|
| `chain_started` | `ExecEnv` begins a run | `scope.chain` | first event of the run | yes | 0 |
| `step_started` | a task **attempt** begins (once per retry) | `scope.{chain,task}`, `task_handler`, `retry` | before every other event of that attempt | yes | 1 unless tool-bearing handler |
| `step_chunk` | one streamed content/thinking delta reaches the sink; or the single coalesced reply on the non-streaming fallback | `content` and/or `thinking` (≥1 non-empty; empty deltas are never published), `model_name`, `provider_type`, `backend_id` | after the attempt's `step_started`, before its `step_stream_end` | **no** | 1 per non-empty field, prose handlers only |
| `step_stream_end` | a model call of the attempt completes successfully — streamed or fallback | `chunk_count` (number of content/thinking parcels, counted independently of `Wants`), `finish_reason` (provider-verbatim; `""` on the fallback, which reports none), `usage{prompt,completion,total}` when the provider reported usage, `model_name`, `provider_type`, `backend_id` | after the LAST `step_chunk` of the call, before the attempt's `step_completed`; **never emitted on mid-stream failure** (`step_failed` closes the attempt instead) | yes — the durable stream bracket | 0 (consumed, not rendered) |
| `step_completed` | a task attempt succeeds | `output_type`, `transition` | last event of a successful attempt | yes | 1 unless tool-bearing handler |
| `step_failed` | a task attempt fails | `error` (`output_type` empty) | last event of a failed attempt; a retrying task emits a fresh `step_started` after it | yes | 1 unless tool-bearing handler |
| `chain_completed` | the run ends successfully | `output_type` | final event of the run | yes | 0 |
| `chain_failed` | the run ends in error | `error` | final event of the run | yes | 0 |
| `chain_suspended` | the run parks on a pending human approval past the fast window and checkpoints | `approval_id` (= the checkpoint key = `scope.tool_call`), `scope.{chain,task,tool_call}` — the full address of the interrupt point | final event of the run **segment**; published only after the checkpoint is durably persisted, so a consumer reacting to it can already resume | yes | 0 (the permission flow's approval card is the suspension UI) |
| `approval_requested` | HITL machinery (hitlservice/localtools) needs a human verdict for a tool call | `approval_id`, `tool_name`, `approval_args`, `approval_diff`, `hook_name`, `hitl_*` | between the affected call's `tool_call_pending` and `tool_call` | yes | 0 (rendered by the permission flow, not the event translator) |
| `hitl_decision` | a HITL policy resolves | `hitl_action`, `hitl_reason`, `hitl_policy_*`, `hitl_matched_rule`, `hitl_timeout_s` | after its `approval_requested` | yes | 0 |
| `tool_call_pending` | execute_tool_calls is about to run one resolvable model-requested call | `tool_name`, `approval_id` = `scope.tool_call`, `approval_args` | before the matching `tool_call` (same `approval_id`) | yes | 1 |
| `tool_call` | one tool invocation finished (success, tool-level error, or unresolvable/hidden/disallowed call) | `tool_name`, `approval_id` (execute_tool_calls paths), `content` (serialized result), `error`, `tool_diff_{path,old_text,new_text}`, `approval_args` | after its `tool_call_pending` when one was emitted (`tools`-handler tasks and error-result paths emit `tool_call` without a pending) | yes | 1 (+ a plan update for `mission_plan`) |
| `print` | a task's `print` template renders after a successful task | `content`; address is chain-level only (no `scope.task`) | after the task's `step_completed` | yes | 1 |
| `token_usage` | before each chat model call (pre-check), and again after a context shift | `token_used` (input-side total incl. tool schemas), `token_size` (budget; 0 = unlimited), `model_name` | inside the attempt, before its `step_chunk`s; at most twice per call | yes | 1 |

## 4. Ordering guarantees

The engine executes one task at a time; events of one run are totally ordered
(per-run bus subject, append-only journal):

1. **Chain bracket:** `chain_started` is the first event; exactly one of
   `chain_completed` / `chain_failed` / `chain_suspended` is the last. A
   suspended-and-resumed run journals one bracket pair per **segment** under
   the SAME `request_id`: `chain_started … chain_suspended`, then (after the
   verdict) `chain_started … chain_completed|chain_failed|chain_suspended`.
   That shared request ID is the documented linkage between the segments —
   replay of one request shows the whole run including where it slept.
2. **Attempt bracket:** every attempt is `step_started` … `step_completed` |
   `step_failed`. Attempts of the same task (retries) and of consecutive
   tasks never interleave. Exception: the attempt interrupted by a suspension
   emits neither — `chain_suspended` (carrying that attempt's task address)
   closes the segment, and the resumed segment re-runs the task as a fresh
   attempt. Likewise the gated call's `tool_call_pending` is left without its
   `tool_call` in the suspended segment; the resumed segment re-emits the
   pending/call pair when the call finally executes.
3. **Stream bracket:** within an attempt, a successful model call emits its
   `step_chunk`s in delta order followed by exactly one `step_stream_end`.
   This holds on the non-streaming fallback too (one coalesced chunk — or
   zero, for a tool-call-only reply — then the bracket). A call that fails
   mid-stream emits no bracket; the attempt ends with `step_failed`. Replay
   therefore always distinguishes "streamed and finished (how, with what
   usage)" from "died mid-stream".
4. **Tool bracket:** per invocation, `tool_call_pending` (when the call is
   resolvable) precedes its `tool_call`; approval/HITL events for that call
   sit between them. Calls of one batch are emitted in the model's call
   order.

## 5. Journaling and replay

`KVJournalTaskEventSink` persists events per `request_id` (key
`taskevents:<request_id>`), after forwarding to the wrapped sink:

- **`step_chunk` is the only unjournaled kind.** Its final text is already in
  chat history; `step_stream_end` is the durable record that streaming
  happened and how it ended.
- Events without a `request_id` are not journaled.
- Large text fields (`content`, `thinking`, `approval_diff`,
  `tool_diff_old_text`, `tool_diff_new_text`) are capped at 16 KiB with an
  explicit `…[truncated]` marker; the live stream carries full values.
- The journal is a ring: the newest 500 events per request are kept.
- `GetJournaledEvents` replays in arrival order, so brackets (§4) hold on
  replay minus the chunks.

## 6. Subscription: sinks, `Wants`, bus subjects

`TaskEventSink.Wants(kind)` is a per-kind subscription filter. Its contract:

- `Wants` gates ONLY whether events are built and published. It must never
  select an execution path — execution semantics are identical whether every
  `Wants` returns true or false (streaming is observation, not a mode).
- Corollary: `step_stream_end.chunk_count` counts the stream's
  content/thinking parcels regardless of whether anyone consumed
  `step_chunk`.
- `BusTaskEventSink` wants every kind while a bus is attached and publishes
  each event to `taskengine.events` and, when the event has a request ID, to
  `taskengine.events.request.<request_id>` (the subject an engine-bridge
  should subscribe to for one run). Bus retention is short (minutes); the
  journal is the durable record.
- `KVJournalTaskEventSink` delegates `Wants` to its wrapped sink (with no
  inner sink it consumes everything) and journals per §5.

## 7. ACP translation

Two translators render events for ACP clients and are kept case-for-case
identical: `Transport.publishEvent` (`internal/surfaces/acpsvc/events.go`)
and `nativeEventTranslator.publish`
(`internal/surfaces/acpsvc/native_turn.go`). The "ACP" column in §3 is their
contract. Translation rules a bridge author should copy:

- `step_chunk` reaches the transcript only for prose handlers
  (`IsAssistantProseHandler`); a route task's streamed output is control
  flow, not narration.
- `step_started/completed/failed` render as a task card only for handlers
  that are NOT tool-bearing (`IsToolBearingHandler`) — tool-bearing handlers
  are represented by their `tool_call_pending`/`tool_call` events instead.
- `step_stream_end` and the chain/approval/HITL kinds are consumed without a
  wire notification (ACP has no end-of-stream frame; approvals render via the
  permission flow). Consumers MUST tolerate — i.e. explicitly ignore — kinds
  they do not render; they must never fail on them or on new additive fields
  such as `scope`.
