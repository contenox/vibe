# Mission Plans — the plan engine as a resident planner

Date: 2026-07-20 (slice status updated 2026-07-21)
Status: building. Slices 1, 2 (store half), 4 (prompt half) and 5 have landed;
see "Slice cut" below for the per-slice status. Companion to
the retired fleet-consolidation record (git history: missions, supervision edge, reports) and informed by
the mission-mode slices landed 2026-07-20 (mission tools, report routing,
`/mission`, inbox).

## The design law (from the first plan engine's postmortem)

The runtime already had a plan engine once: `plancompile`/`planservice`/
`planstore` (last live at `0c28a69^`, 2026-05-09 — recover any of it with
`git show 0c28a698eafb2649d9727e962520db7e88919342^:runtime/planservice/planservice.go`).
It compiled plans into task-chain DAGs and died of exactly that:

> a plan is never going along plan, and enforced via DSL gave it zero
> flexibility without an orchestrator LLM that can replan.

The law that follows, stated once and binding on every design below:

**Never compile a plan into control flow. A compiled plan is a prediction
enforced as law. The plan engine IS a dedicated planner LLM in a resident
loop — planning is its only job, and the plan stays alive because that one
mind never stops holding it.**

- **Single writer.** One planner owns the plan. Workers never touch it; they
  execute bounded missions and file reports. Distributing plan-writing into
  workers fragments ownership and kills the plan slowly instead of quickly.
- **The old planner was one-shot** (plan, then dead, with a DSL corpse
  enforcing the prediction). The fix is not replanning bolted on anywhere —
  it is a planner that never exits: report-in → plan revised → next mission
  fired. The supervision edge is the planner's heartbeat.
- **Determinism only at boundaries**: envelopes bound what may happen,
  reports and terminal status are the hard facts planning reasons over, the
  plan artifact is the reviewable record. Judgment lives inside; rigid truth
  at the edges.
- **Drift is surfaced, never prevented.** A replan is an attention-worthy
  event (inbox: "plan revised +2/−1 — <explanation>"), and landed-vs-planned
  is reviewed after the fact. The old engine chose "deviation impossible" and
  died of it.

External validation: OpenHands deleted their special `PlannerAgent` classes
and DAG machinery and converged on plan-as-living-file + constrained
capability profiles; task-master kept the compiled-DAG road and pays for it
with an 1,860-line dependency-repair subsystem and a next-task scheduler —
the counterexample that proves the law from the other side.

## ACP boundary (verified against the vendored rust-sdk, `schema::v1`)

ACP **mandates nothing** about plans. The entire wire surface is:

- `SessionUpdate::Plan` — agent→client, **full-snapshot** entries
  `{content, priority: high|medium|low, status: pending|in_progress|completed}`.
  Each update replaces the whole list. Optional; render-only. The types
  already exist in `libacp/plan.go`.
- **Session modes** are protocol-standard *plumbing* with agent-defined
  *semantics*: `SessionModeState` (advertise), `session/set_mode` (client
  switches), `CurrentModeUpdate` (agent switches). If a plan mode is exposed
  to editors it goes through this machinery — never `_meta`.

Everything else — the planner loop, ownership, revision, recovery — is
off-wire and unconstrained. Contenox-specific attribution stays in `_meta`
(the sanctioned extension channel, as `contenox.missionReport` already does).
Wire shapes get pinned in `TestConformance_` (the stub already advertises
modes via `ACP_STUB_ADVERTISE_MODES=1`).

## Ontology

- **Plan**: an ordered list of entries held on a mission record, owned by
  exactly one planner session. Projected to ACP as full-snapshot `plan`
  updates. Optionally materialized as a markdown checklist file (the old
  `plancompile` grammar: `**Goal:**` + `- [ ] 1. …` — `ParseMarkdown` ↔
  `renderMarkdown` round-trips; checklist state ↔ entry status is a
  bijection). The file is a review/versioning affordance, never an execution
  input.
- **Planner**: a declared agent whose envelope grants plan tools + mission
  verbs and withholds execution tools. Not a new kind, not a new class —
  a capability profile (the OpenHands lesson: constrained toolset + boundary
  prompt + plan artifact suffices; enforce the boundary at the *tool* layer,
  the prompt is advisory). Workers get the inverse: mission tools, no plan
  tools.
- **Reports** are the planner's senses. They adopt the old summarizer's typed
  handover shape — `{outcome, summary, artifacts, handover_for_next,
  caveats}` — so a landed mission hands real context to the next one instead
  of prose. Reports remain append-only journal entries; the runtime owns the
  envelope (ids, timestamps), the model owns only the substance
  (task-master's `<info added on …>` discipline, which our report mechanism
  already matches).
- **Terminal status** is a closed set on the mission: at minimum
  `landed | derailed | stuck` alongside the running states. `stuck` is a
  first-class terminal signal distinct from failure (OpenHands treats
  STUCK == ERROR at the boundary; the detection heuristic is the planner's
  business, the discrete status is the runtime's).

## Patterns adopted from the OSS mining (2026-07-20)

Mined: openai/codex, zed-industries/claude-code-acp, claude-task-master,
OpenHands, sst/opencode, charmbracelet/crush.

1. **`explanation` on every plan revision** (Codex `UpdatePlanArgs`): an
   optional rationale string attached to each snapshot. It is what the
   inbox's "plan revised" event carries. Cheap, forced honesty on scope
   pivots.
2. **Anti-echo rule** (Codex prompt): the planner must not restate the plan
   in prose after updating it — the harness renders it. Prevents a stale
   fork of the living document.
3. **Status-transition discipline as prompt text, not host enforcement**
   (Codex): exactly one `in_progress`; no pending→completed jumps; no
   batch-completions after the fact; finish with everything completed or
   explicitly deferred. Codex ships worked high/low-quality plan examples
   (5–7-word steps, no filler, no single-step plans) — adapt that text for
   the planner's system prompt. Host-side we validate *shape* (parse,
   deny-unknown-fields) and stay soft on discipline, as Codex does.
4. **Snapshot reconciliation at the boundary** (claude-code-acp `TaskState`):
   if the planner ever emits incremental operations, fold them into a full
   snapshot before the wire; deletion = omission from the snapshot, never a
   wire status.
5. **Immutable completed work** (task-master's update guardrails): revision
   never mutates done entries; corrections are appended as new entries. This
   is what makes continuous replanning audit-safe.
6. **Analyzer→expander split** (task-master): a cheap critique pass that
   emits a per-item decomposition prompt + recommended breadth, consumed by
   a separate expansion step. The pattern for a planner deciding when a
   mission needs sub-missions — as planner judgment, not as machinery.
7. **Mode = permission ruleset, doubly enforced** (opencode): a mode both
   *hides* denied tools from the model's schema and *gates* them at runtime.
   Our envelopes are that ruleset; ACP `session/set_mode` swaps it. The one
   opencode mistake not to copy: their bash read-only-ness in plan mode is
   prompt-only — every boundary here is engine-enforced (same stance as the
   control-plane isolation invariant).
8. **Structured work-handoff envelope** (task-master
   `TaskImplementationMetadata`): `relevantFiles {path, action}`,
   `scopeBoundaries {included, excluded}`, `acceptanceCriteria` — the shape
   for a mission intent's structured half when the planner fires workers.
9. **Bounded planner memory** (OpenHands condenser): the resident planner's
   session needs compaction that anchors the goal + current plan and
   compresses the report history. The plan artifact and the planner's
   conversation memory are distinct things; the artifact is what makes the
   loop rehydratable after process death.
10. **Parent/child roll-up** (OpenHands `sub_conversation_ids` ≈ our
    `ParentSessionID`): the batch is a planner mission with child missions;
    listing, roll-up, and cascade teardown follow the edge we already record.

Deliberately not adopted: task-master's DAG-as-scheduler and dependency
auto-repair (the law forbids the premise), OpenHands' human-click-Build as
the only replan trigger (our planner replans off reports; humans gate via
envelopes and the inbox), any plan-conformance *enforcement* (plan-as-
contract is a separate direction, below).

## Slice cut

1. **Terminal status + plan record** (`missionservice`): closed status set;
   `Plan` entries (id, content, status, priority, revision counter) +
   `explanation` per revision; revision events on the bus (the inbox
   consumes them like reports). — **LANDED** (`missionservice`: `Status`,
   `Plan`/`SetPlan`, `PlanRevisedEvent`, `Finish`/`StatusChangedEvent`).
2. **Plan tools + projection**: `mission_plan` set/update (same `_meta`
   grant + scoping as `mission_report`, planner-profile only); ACP
   full-snapshot projection on the owning session; validation port of
   `planner_validate.go`. — **STORE HALF LANDED** (`missiontools.mission_plan`
   → `SetPlan`, full-snapshot echo with id carry-forward; the plan validation
   port lives in `missionservice`). The ACP full-snapshot `plan` projection on
   the owning session is still open.
3. **Inbox/board rendering**: per-mission step progress; "plan revised"
   entries in the triage feed with explanations. — **IN PROGRESS** (the Beam
   TUI's mission panel).
4. **Planner profile + prompt**: a declared planner agent (envelope grants
   plan tools + mission verbs, withholds execution tools) with the adapted
   Codex discipline text; the batch journey (plan → fire → skim → assess)
   documented as the acceptance. — **PROMPT HALF LANDED**: the profile ships as
   `internal/surfaces/contenoxcli/agent-planner.json` (discovered by the agent-* convention,
   granted only the `mission` tools, withholding execution tools) with the
   Codex-derived discipline prompt. See the appendix for the prompt text and the
   envelope guidance. Sub-mission firing from the profile is a FUTURE slice.
5. **Typed handover on reports** + report-driven revision loop e2e: a real
   planner unit revises its plan off a worker's report (composed test in the
   `e2e_mission_roundtrip` idiom). — **LANDED**: `missionservice.Report` grew an
   optional typed `Handover` (`{outcome, artifacts, handoverForNext, caveats}`,
   additive, absent = legacy report), carried on `mission_report` and on
   `ReportAddedEvent`; `mission fire --wait` is kind-aware (result → 0, blocker →
   2, progress/finding → keep waiting); the revision-loop acceptance is
   `fleetservice/e2e_mission_revision_loop_test.go`.

Open directions, explicitly not in these slices: plan-as-contract (plan
steps scoping envelopes — blueprint-first, feeds the state-diff
criterion: landed-vs-planned is its concrete test), plan-file round-trip
tooling, and modes surfaced to editors via `session/set_mode`.

## Appendix: the planner profile (slice 4, prompt half)

The planner is not a new agent KIND — it is a capability PROFILE (the OpenHands
lesson: constrained toolset + boundary prompt + plan artifact suffices). It ships
as a declared chain agent, `internal/surfaces/contenoxcli/agent-planner.json`, scaffolded
into `~/.contenox/` by `contenox init` and declared as a fleet-dispatchable agent
by the `agent-*` filename convention (chain-agent discovery). Fire it like any
mission: `contenox mission fire --agent agent-planner --intent "…" --policy …`.

**The envelope.** The profile's `chat_completion` tasks grant exactly the
`mission` tools provider and NOTHING else — `"tools": ["mission"]`, with no `"*"`,
no `local_shell`, no `local_fs`, no `webtools`. That is the whole envelope: the
planner holds `mission_plan` / `mission_report` / `mission_finish` /
`mission_ask_attention` (each scoped to its own mission id, granted at session
construction) and has no way to execute. Withholding execution tools at the TOOL
layer is the enforced boundary; the prompt below is advisory discipline on top of
it (blueprint pattern 7 — the ruleset is engine-enforced, the discipline is
prompt-only). Workers get the inverse profile: execution tools, no plan tool.

**Firing sub-missions is a FUTURE slice.** The planner cannot yet spawn worker
missions — there is no sub-mission tool in its grant, and the prompt says so
outright. For now a planner holds and evolves its own plan and reports; the
analyzer→expander decomposition (blueprint pattern 6) and parent/child roll-up
(pattern 10) that let a planner fire workers are a later slice.

**The discipline prompt** (canonical copy: the `plan_loop` task's
`system_instruction` in `agent-planner.json`; reproduced here with real
newlines):

```
Current date: {{date}}.

You are the PLANNER for a mission. Your one job is to hold and evolve a living
PLAN while the mission runs. You run UNATTENDED: no human is reading this
conversation, so prose reaches no one. You record your work and reach your
operator ONLY through your mission tools. You have no execution tools by design —
you plan, you do not execute.

MAINTAIN THE PLAN with mission_plan. Every call sends a FULL SNAPSHOT of the
entire plan, never a delta: an entry you leave out is deleted, an entry you
include is kept or updated. Carry an entry forward by echoing the `id` you were
given back; introduce a new one by omitting `id` (the runtime assigns it and
returns it). Use the ids the tool echoes back on your next revision.

PLAN DISCIPLINE — these are YOUR rules to keep; the runtime validates shape but
will not enforce discipline for you:
- Exactly ONE entry is in_progress at any time. Finish or hand off the current
  step before you start the next.
- No entry jumps straight from pending to completed. A step becomes in_progress
  before it becomes completed.
- Do not batch-complete steps after the fact. Mark a step completed when it is
  genuinely done, not in a cleanup pass at the end.
- Completed work is immutable. To correct a finished step, append a NEW entry —
  never rewrite the old one's text.
- Keep steps short and real: a few words each, no filler. Do not write a
  single-step plan — one step is not a plan.
- Give a one-line `explanation` on every revision that changes the plan's shape:
  what pivoted and why. It is the record of your reasoning the operator will read.

NEVER RESTATE THE PLAN IN PROSE. After you update it, do not repeat it back in a
message — the harness renders the plan for the operator, and a prose copy forks a
stale version of the living document. Say only what a plan snapshot cannot: a
decision, a question, a result.

REPORT with mission_report. File a `finding` for something worth the operator's
attention, a `blocker` when you hit something you must not decide alone, and a
`result` when the work is done. On a `result` that a FOLLOW-UP mission will build
on, fill the typed `handover` (outcome, artifacts, handoverForNext, caveats) so
the next unit starts from real context instead of re-deriving yours.

END with mission_finish exactly once, when the work is truly over: `landed` on
success, `derailed` on failure, `stuck` when you have hit a wall you cannot get
past unattended. Prefer mission_ask_attention while there is still work to resume.

Firing sub-missions to workers is not yet yours to do — that capability is a
later slice. For now: hold the plan, keep it honest, and reach the operator
through your tools.
```

The prompt composes with `fleetservice`'s unattended preamble/nudge (which every
dispatched unit's first turn carries): the preamble names the mission tools and
says prose reaches no one; this prompt goes further and gives the planner its plan
discipline. The two agree — neither contradicts the other — because both are built
from the same fact: a mission unit reaches its operator only through mission tools.
