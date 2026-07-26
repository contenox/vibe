# Eino evaluation — replace vs learn (synthesis, 2026-07-26)

Two adversarial evaluations of cloudwego/eino (Apache-2.0, v0.9.13 line) as a
candidate for our chain-execution kernel: one agent argued the strongest
replace case (score: 6/10), one the strongest learn/mine case (score: 8/10).
This memo is the synthesis and the decision record.

## Where both advocates agree (the load-bearing facts)

1. **Eino's streaming discipline is correct and ours is broken in exactly the
   way it prevents.** Their assembly is centralized (one `concatToolCalls`,
   index-grouped, hard errors on mismatch) and providers emit raw deltas
   only; paradigm can never change semantics because stream↔invoke
   adaptation is one shared code path. Our C1 (streamed tool calls dropped
   on 7/9 providers) exists because assembly was distributed to nine
   adapters under a "normally"-worded contract, and because
   `eventSink.Enabled()` forks execution onto a different code path.
2. **Eino's interrupt/checkpoint/resume is categorically ahead of our parked-
   goroutine HITL.** An interrupted eino run is a row in a store — no
   goroutine, survives restart. Our approvals hold a goroutine + full
   history in RAM up to 1h and lose the run on restart.
3. **Most of our engine is not orchestration.** ~60–70% of taskexec is
   policy eino does not provide (token budgeting/shift, tool-result caps,
   allowlist grammar, pairing repair, MacroEnv, route normalization,
   candidate resolution). Under replacement all of it becomes adapter code
   we still own — against a pre-1.0 API with visible deprecation churn.
4. **Our chains-as-data is the fork in the road.** Eino's guarantee stack
   presumes graphs authored in Go. A chain-JSON→eino compiler is feasible
   (est. 1.5–2.5k LOC + 3–4k LOC of adapters, 3–5 dev-months to parity),
   but DSL chains would flow as `any` through their graph — surrendering
   the compile-time-checking headline for exactly the chains we'd compile —
   while a load-time chain linter lands *easier* in our closed six-value
   DataType world than in their open-type world.
5. **`on_failure` has no eino primitive.** Our per-node failure routing
   (used by 5 of 12 shipped chains) needs error-boxing wrappers that also
   bypass eino's error telemetry.

## Where they differ

The replace case's remaining edge is the **ecosystem**: eino-ext's 16
maintained provider components (verified: their providers stream tool calls
correctly), the ACP bridge, the adk multi-agent layer, ByteDance-scale
battle-hardening of the concurrent stream runtime. The learn case concedes
this is the ~2 points it cannot capture: transfers take invariants, not
maintained code, and we keep owning nine provider adapters.

## Decision: LEARN

The learn path captures the four highest-value properties — centralized
assembly, load-time validation, interrupt/checkpoint, composition rules — as
invariants that fit our architecture better than the framework itself would,
and it **sequences** (the cheap transfers enable the big ones) where
replacement is all-or-nothing. The modeld test confirms: the engine is our
differentiator, it is not cleanly separable (the weave), and eino is better
only at mechanisms we can transfer.

**Revisit trigger (written down on purpose):** if the roadmap turns toward
arbitrary parallel typed dataflow — fan-in merges, field mappings, DAG
scheduling — the learn path degenerates into reimplementing eino piecewise,
and replacement (or embedding eino as the executor under a chain compiler)
wins that terrain. Reopen this memo at that point.

**Amendment (same day, maintainer):** the current DSL is NOT a contract —
the requirement is only "meaningful chains that are okayish to author." This
narrows the gap (replace ≈7: a fresh format designed for eino's primitives
dissolves the on_failure-preservation blocker) but does not flip the decision:
the decisive costs — the weave and the ~4k LOC of policy taskexec carries
that eino lacks — are DSL-independent, and format freedom helps the learn
path equally. Consequence adopted: the planned spec reframe becomes **chain
format v2** — terse and pleasant to author, with declared IO signatures,
linter validation (T3), and hierarchical addresses (T5) designed in from day
one, and its primitives kept deliberately close to eino's node/branch/
interrupt shapes so that acting on the revisit trigger would be a compiler
weekend, not a rescue.

## The transfer queue (adopted into TODO)

| # | Transfer | Effort | Retires |
|---|---|---|---|
| T1 | Centralized delta assembly: raw-delta provider contract (`{ContentDelta, ThinkingDelta, ToolCallDelta{Index,ID,Name,ArgsFragment}, Usage}`), one engine-side assembler (concat-by-index, atomic-field consistency, hard errors wrapped with provider context), typed terminal parcel, per-provider golden SSE fixtures | M | C1 *structurally* (not per-provider patches) |
| T2 | "Paradigm never changes semantics": one execution path, streaming is observation; replace boolean `Enabled()` with per-kind `Wants(kind)` | S | the switch that armed C1 |
| T3 | Chain linter at load: handler signature registry (closed table: e.g. chat_completion {string,chat_history}→chat_history), DataType dataflow walk over goto/on_failure edges with eino's tri-state (must/mustNot error at load, may keeps runtime check), input_var/macro reference checks, teaching errors naming both endpoints, sticky disable on the chain row; wired into taskchainservice (write+read), chainagents.Discover, a `chain vet` verb, ExecEnv backstop | M | the whole runtime SEVERBUG class; unvalidated chains seeding the agent registry |
| T4 | Interrupt/checkpoint: HITLWrapper third outcome `ErrApprovalPending`; JSON checkpoint of {vars+types, edgeCounts, history, pendingToolCalls} keyed by approvalID with **hierarchical addresses** (chain/task/toolCall) and a versioned envelope + migration hook from day one (their v0.3.26 lesson); resume re-enters via the existing tool-pairing repair path; hybrid policy: park ≤30s fast path, checkpoint-and-release slow path; nativeturn gains `suspended` | L | runs lost on restart; hour-long goroutines per approval; missions unable to detach on ask-attention |
| T5 | Event-contract hardening: terminal `step_stream_end` event (chunk count + usage), per-kind field matrix documented as THE engine-bridge contract, event scope → hierarchical address | S | beam engine-bridge contract gaps; replay missing stream brackets. Prerequisite for T4 |
| T6 | `call_chain` handler: chains declare input_types/output_type in their envelope; child signature = node signature checked by T3's linter; include-cycle detection at load; ACP self-spawn stays as the deliberate isolation tier | S–M | composition costing an OS process and losing types at the boundary |
| T7 | Stream hygiene: ctx.Done() in every relay send, explicit copy/close obligations | S | llmrepo relay goroutine leak |

Dependencies: T5 before T4; T3 before T6. T1+T2 merge INTO the provider fix
slice — do the structural fix, not the 20-line patch.

## Never-do list (from eino's own scars)

No global mutable process-init registries in a hot-loading daemon · never
panic at runtime type boundaries (keep teaching errors) · no reader-copy
fan-out with close-obligation-by-convention (bus fan-out stays) · keep the
DataType enum closed — no reflect in the chain schema · versioned checkpoint
envelope + round-trip tests on every field (gob silently dropped a *int and
corrupted concat-after-resume for them) · wrap assembler errors with
provider/model context before they reach users · don't import dual execution
modes / fan-in machinery ahead of demonstrated need · design interrupt
addresses + typed state in v1 (theirs is on its second API generation
because they bolted addresses on).
