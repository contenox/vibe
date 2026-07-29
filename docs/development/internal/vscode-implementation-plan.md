# VS Code extension: implementation plan — 2026-07-28

Supersedes the first draft of this file. Companion to
[vscode-surface-advisory-brief.md](vscode-surface-advisory-brief.md) (the
question) and [vscode-surface-advice-fable.md](vscode-surface-advice-fable.md)
(an advisor's answer — §3 of which this plan overturns, see "What this kills").

**The deliverable is the chat panel.** Everything below is either a panel
feature or the thing that unblocks one. Plumbing that serves no panel feature
is not in this plan.

**V1 scope (maintainer, 2026-07-28): everything you would expect of a
Contenox VS Code extension ships in V1.** Phases 1–7 are in. The bar is not
"the port works" — it is that a developer installing this finds the assistant
complete: context they can see and control, code that reaches the editor,
sessions that persist, approvals they can answer, and honest state (usage,
turn status, why a call was gated).

**[Wellworn](auto-bookmarks-vscode-plan.md) is NOT V1.** It is a later
project. Nothing in Phases 1–7 depends on it, and its cost is judged on its
own — see the correction in this doc about chain switching, which buys it
nothing.

---

## 1. The diagnosis

The panel is a web-admin chat SPA transplanted into a webview. Its props still
carry Bob SaaS concepts (`appCount`, `canManage`, `searchReady`, `sourceCount`,
`syncedSourceCount`). It has no context attachment, no way to get code into the
editor, no slash commands, no images, no model picker, no usage indicator, and
no rendering of *why* a call was gated.

The cause is not neglect. **The bespoke bridge dialect is narrower than ACP**,
so every panel feature costs a hand-designed RPC plus a hand-wired webview
message. That cost is why features are missing, and it compounds: the bridge
already emits 9 notifications and the webview protocol only has message types
for 4. `hitlDecision`, `contextUsage`, `chatStarted`, `chatCompleted`,
`chatFailed` and `chatCancelled` are computed and thrown away one layer short of
the screen.

## 2. Every missing panel feature is already core ACP

Checked against `libacp`, not assumed:

| Panel gap today | Already on the ACP wire |
|---|---|
| Context attachment is `content: string` plus an **expiring** `pendingContext` side-channel | `resource_link` content blocks (`content.go:12,55`) — typed URI + name, in `session/prompt` |
| No images | image content blocks; `acpsvc/images.go` already serves them |
| 13 slash commands unreachable from the panel | `available_commands_update` → `CommandsUpdated` |
| No context/token indicator | `usage_update` → `UsageUpdated`, with `UsageCost` |
| No "why was this gated" | `session/request_permission` `_meta` → `approvalflow.Meta`: tool, named policy, diff |
| No busy/failed/cancelled turn states | `StopReason` → `TurnEnded` / `TurnFailed` |
| No plan/todo view | `plan` → `PlanUpdated` |
| No model picker in the composer | `SessionModelState.AvailableModels` + `session/set_model` |
| No resume after walk-away | `session/resume` — already implemented at `enginebridge/bridge.go:467` |
| No mission/inbox surface | `MissionReport`, `MissionAsk`, `MissionStatusChanged`, `MissionPlanRevised`, `InboxItemAdded` |
| Unknown future kinds break the panel | `UnknownUpdate` — forward-compatible by construction |

The bespoke dialect reinvented narrower versions of `usage_update`,
`available_commands_update` and the permission flow, and simply never got round
to the rest.

## 3. What is genuinely VS Code-only

Small, and ACP has a first-class mechanism for it: extension methods, namespaced
`_contenox/*`, handled via `SetExtRequestHandler` / `SetExtNotificationHandler`
on both connection sides (`libacp/methods.go:56-76`, `conn.go:177-190`). We
already ship one in production — `_contenox/terminal/run`.

- **`_contenox/autocomplete`** — FIM. The one real protocol gap; ACP has no
  fill-in-the-middle concept. Today ~105 LOC in `vscodeagent/chat.go:462-566`.
- **`_contenox/websearch`** — if it stays client-driven (`websearch.go`, 187
  LOC); otherwise it is a tool and needs no protocol at all.

**Apply-to-file, insert-at-cursor and copy need no protocol.** They are VS Code
API calls on a code block the panel already renders. They are missing because
nobody wrote them, not because the wire can't carry them.

## 4. The architecture

ACP is the core. The extension is an ACP client. VS Code-only needs ride
`_contenox/*` extension methods on the same connection.

```
VS Code extension (TypeScript)
  = ACP client  +  _contenox/* extensions (autocomplete)
        │  stdio, ACP
        ▼
  contenox acp   ──▶  acpsvc.Transport  ──▶  engine / services
```

Two consequences worth stating plainly:

- **The panel stops being starved.** New panel features become "render what is
  already on the wire" rather than "design an RPC, add a webview message type,
  wire both ends."
- **One session contract, three surfaces.** The terminal UI already consumes
  `acpsvc` through the in-process loopback in `enginebridge` (which imports
  **zero** beamtui packages — it is already surface-agnostic). Zed and JetBrains
  consume the same thing over stdio. The extension becomes the third consumer of
  one tested path instead of the second implementation of a private one.

`enginebridge` should move to `internal/surfaces/enginebridge/` when a second
surface uses it. A surfaces-layer move, not a kernel change.

## 5. Phases

Each phase ends in something visible in the panel.

**Phase 1 — Transport.** Settled by
[acp-client-ts-spike.md](acp-client-ts-spike.md): adopt the official SDK,
npm `@agentclientprotocol/sdk` (`@zed-industries/agent-client-protocol` is the
same project under its deprecated old name). It supports the client role and
arbitrary `_contenox/*` methods, and a live spike completed
`initialize` → `session/new` → `session/update` → `session/delete` against a
real `contenox acp`. Fallback if it disappoints: a hand-written client, sized
at 800–1400 LOC.

Three consequences:

- **The extension host must be bundled.** The SDK is ESM-only, the host is
  CommonJS, and `.vscodeignore` strips `node_modules/**`. Extend the esbuild
  pattern already used for the webview to `src/extension.ts`. This gates
  everything else in the phase.
- **`packages/vscode/src/bridge/*` is dead.** It uses Content-Length framing
  (ACP is NDJSON), a different method dialect, talks to `contenox vscode-agent`
  rather than `contenox acp`, and has the cancel method name wrong
  (`$/cancelRequest` vs. ACP's `$/cancel_request`). None of it is reusable.
  Delete it with the port, do not adapt it.
- **`_contenox/autocomplete` already exists** on the agent side
  (`internal/surfaces/acpsvc/autocomplete.go`, ported field-for-field from the
  old bridge, 12 unit tests). It still needs its FIM chain registry wired in
  `contenoxcli/acp_cmd.go`.

Then port the panel onto ACP session lifecycle, prompt, and `session/update`.
Keep the webview's own host↔webview message shape where it is already fine;
what changes is what feeds it. *Exit: feature parity with today, on ACP, with
the extension tests still green.*

**Extra exit criterion — the panel must inherit the TUI's answer quality, which
means inheriting its chain.** The terminal UI is a genuinely polished
codebase-Q&A experience; that quality is not free and not generic. `contenox
new` loads `chain-beam.json`; `contenox acp` loads `default-acp-chain.json`
from `chain-acp.json`. Same ten tasks, same structure — but the beam chain's
prompts are far richer: `coding_chat` 6,465 vs 2,897 bytes (more than double),
`acp_chat` 3,969 vs 2,581, `coding_recovery` +598, `summarise_failure` +219.
That delta is the coding doctrine.

The extension spawns `contenox acp`, so **by default it inherits the thinner
chain and will answer worse than the TUI** — and it will read as a UI
regression when it is a prompt difference. Options, cheapest first: set
`CONTENOX_ACP_CHAIN_PATH` when spawning (zero protocol work); or close the gap
in `chain-acp.json` itself; or per-session chain selection (see the Wellworn
plan's option B, which needs the same mechanism).

**Do not "fix" this by lifting the beam doctrine (maintainer, 2026-07-28).**
The beam chain is expensive — it eats tokens freely because an attended
terminal session on a large hosted model can afford to. An editor chain cannot
inherit that: it has to stay usable against Ollama and other local models, so
the VS Code chain will be its own thing, tuned for cost, not a copy of beam's.
Designing it is deliberately **not** a now-topic.

Also note the audience argument is void: enriching `chain-acp.json` must not be
justified as "this helps Zed and JetBrains too". The ACP registry has merged
zero independent agents in its last 100 PRs and PR #353 is closed (WORK.md
item 10), so that audience is hypothetical.

What Phase 1 must do is only this: **make the chain a knob, not a constant.**
The extension spawns the process, so it can set `CONTENOX_ACP_CHAIN_PATH` to
whichever chain it should run — zero protocol work, available today. Record
which chain the panel ran with, so answer quality is attributable to a chain
rather than blamed on the port.

**Switching chains does not need a protocol picker (maintainer, 2026-07-28).**
A slash command or a VS Code command is enough. The cheapest form needs no
runtime change at all: a VS Code command respawns `contenox acp` with a
different `CONTENOX_ACP_CHAIN_PATH`. Sessions are durable in SQLite and
`session/load` exists, so the session survives the restart. A `/chain` slash
command is the same idea one layer in — swapping the transport's chain for the
process rather than per session.

Correction to an earlier note in this doc: this is **not** the same requirement
as the Wellworn plan's option B, and they should not be conflated. Chain
*switching* is one-at-a-time user preference — process-wide is fine. Wellworn
needs a *concurrent* second chain: an explain session running a tool-using
chain while the chat session keeps its own, at the same time. A command that
swaps the process's chain cannot serve that. So the cheap answer here does not
buy Wellworn anything, and Wellworn's cost must be judged on its own.

Background, unchanged: `ChainRegistry` holds one `defaultChain`
(`acpsvc/chain.go:23-26`) and the advertised config options are
model/provider/max-tokens/think — no chain among them.

**Extra exit criterion — the pickers come from ACP and offer valid options.**
Not parity with any other surface: the extension is its own session and may
sit on a different model than the TUI by choice. What must hold is that it
resolves *valid* options and drives them through the protocol —
`SessionModelState.AvailableModels` and `session/set_model` for the model
picker, `session/set_config_option` for the rest — instead of a bespoke
catalog.

Today it does neither. The panel shows `vertex-google (not configured)` and a
run dies with `no models matched requirements: provider: ["vertex-google"]`,
because `vscodeagent` is the **only** surface not going through
`acpsvc.Transport` (the TUI reaches it via the `enginebridge` loopback;
`contenox acp` and `fleetboot` use it directly) and reimplements
`listProviders`/`listModels` with its own `configured` heuristic
(`server.go:477`). Deleting that reimplementation in favour of the ACP
session-model state is the fix. Do not repair it in the bridge — dying code.
Verify after the port with the visual harness, which already surfaced it.

**Phase 2 — The four free features.** Usage indicator, slash-command menu, turn
states, and the gate reason on approval cards. All four are already on the wire
after Phase 1; this is rendering work only. *Exit: the panel shows why a call
was gated, and what the context costs.*

**Phase 3 — Real context.** Attachment chips from `resource_link`: `@file`,
`@symbol`, current selection, active file, drag-drop, image paste. Visible,
removable, no TTL. *Exit: you can see and control exactly what the model was
sent.*

**Phase 4 — Code out of the panel.** Apply-to-file, insert-at-cursor, copy,
per-hunk accept on diffs. Pure client-side. *Exit: the panel is useful for
writing code, not just discussing it.*

**Phase 5 — Walk-away.** `session/resume` in the panel; suspended state and a
resume affordance; answer-out-of-band then reopen and watch it finish. *Exit:
park a gated call, quit VS Code, answer from a terminal, reopen — the run
completes in the transcript.*

**Phase 6 — Inbox and missions.** Activity-bar badge on pending asks; mission
status and plan, read-only. Both vocabularies already exist.

**Phase 7 — Polish and distribution.** Strip the SaaS readiness props; model
picker in the composer; retry / edit-and-resend; bridge respawn with backoff;
wire-or-delete `requestNativeApproval`; fix the docs' `@contenox` participant
claim; Open VSX; consolidate the duplicated 5-target release matrix.

## 6. What this kills

- **The bespoke 18-method dialect**, once `_contenox/autocomplete` covers the
  only thing it uniquely carried.
- **fable's §3** ("keep the bespoke bridge; converge only the machinery"). Its
  reasoning was that ACP's wire shape would fight FIM and the VS Code-shaped
  methods. ACP's extension mechanism is built for exactly that case, this repo
  already uses it, and the dialect is what starved the panel. The rest of that
  document stands.
- **The first draft of this plan**, which proposed changing `enginesvc` to
  compose event sinks. Surface-local problem, kernel-wide change, wrong. The
  standing constraint below exists because of it.

## 7. Standing constraint

Work stays inside `internal/surfaces/` and `packages/vscode/`. If a step appears
to need a change under `internal/kernel/` or `internal/services/`, that is the
signal the step is wrong — re-derive it. The kernel is shared by the CLI, the
terminal UI, ACP editors and the fleet.

## 8. Open questions

- **Does the panel's host↔webview protocol survive?** Phase 1 assumes the
  webview keeps its current message shape and only its feed changes. Verify
  before committing to it; if the shape has to change, Phase 1 grows.
- ~~**TypeScript ACP client: build or vendor?**~~ ANSWERED — see
  [acp-client-ts-spike.md](acp-client-ts-spike.md). Adopt the official SDK.
- **Does `internal/surfaces/vscodeagent` survive at all?** The extension will
  spawn `contenox acp` directly, so the bespoke Go bridge has no client once the
  port lands. Retire it in the same change that deletes `src/bridge/*`, or keep
  it until the port is proven and delete both together — but do not maintain two
  session paths past Phase 1.
- **Bundle size and Zod validation against live responses.** The SDK validates
  with Zod; our agent's real payloads have not been run through it end to end.
- **`engines: ^1.96.0`** may under-declare if `LanguageModelChatProvider`
  finalized in 1.104.
- **D6, separately:** `NewKVJournalTaskEventSink` has no production call site
  while `engine-events.md` §5 calls it the durable record. Kernel decision,
  deliberately not folded in here.
