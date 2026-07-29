# VS Code surface advice (fable) — 2026-07-28

## 1. Recommendation

Make the extension the primary *attended* surface — primary meaning where polish
attention goes, never where survival depends. Keep the bespoke `vscodeagent`
bridge; do not converge the extension onto ACP. But before any new feature,
fix the spine defect the restore carried in: the bridge is the only surface
that unplugs the engine bus (`vscodeagent_cmd.go:277` →
`enginesvc/engine.go:183-187`), it drops `chain_suspended` and
`approval_requested`, and it has no resume path — so "an agent you can walk
away from" is structurally false in exactly the surface you're promoting.
Ship walk-away in the IDE first, publish to Open VSX second, polish the chat
webview third. Keep beam alive and feature-frozen as the un-gated attended
fallback. Retire WORK.md's "connector-only shim, last" line — it is already
superseded by a live Marketplace listing at v0.36.0.

## 2. What the extension should be

The intent quote asks for "everything you expect out of an IDE assistant."
In 2026 that bar is set by Copilot, Cursor, Continue, and Cline, and most of
it already exists in `packages/vscode`: streaming chat with tool cards and
diff-backed approval cards (`src/chat/ChatWebviewViewProvider.ts`), sessions
and history (`src/chat/SessionTreeProvider.ts`), editor context actions
(ask/fix selection, fix/explain diagnostics, review changes, draft commit
message — `src/editor/context.ts`, `src/codeActions/diagnostics.ts`), inline
FIM completion (`src/autocomplete/provider.ts`), a native model-picker
registration (`src/lm/provider.ts`), an MCP server definition provider, and a
walkthrough. The gap to "top tier" is not missing features. It is that the
existing features are unfinished at the edges and the one thing no competitor
has is broken here.

**Table stakes (absence is disqualifying).** Reliable chat with streaming and
cancel — exists. Approval cards with diffs — exists, fail-closed, tested
(`src/test/extension.test.ts` covers the permission protocol over real
framing). Sessions — exists, but rename is hardcoded unsupported
(`ChatWebviewViewProvider.ts:265`) while acpsvc has `/rename`; the bridge
needs the RPC. Editor context attach — exists. Inline completion — exists,
off by default, correctly gated. Commit-message drafting — exists.
Crash recovery — half-exists: on child exit the extension sets a crashed
status and waits for a human (`BridgeProcess.ts:174`); one automatic respawn
with backoff is table stakes for a long-lived assistant.

**Differentiators (contenox's own, and the actual reason to build this).**
First: *walk-away*. Park-window approvals, durable checkpoints,
`chain_suspended`, out-of-band answering, exactly-once resume — the runtime
does all of it (`localtools/hitl.go:318-407`, `taskenv.go:199`,
`agentservice/resume.go`) and the extension surfaces none of it. This is the
feature Copilot and Cursor cannot copy, and it is the whole §1 sequencing
argument. Second: the operator inbox as a native VS Code experience — an
activity-bar badge on pending asks, answerable after the turn ended, backed
by the same `operatorinbox` events beam already watches
(`beamtui/enginebridge/inbox.go`). Third: honest policy rendering — the
`hitlDecision` notification already carries policy name, matched rule, and
timeout (`vscodeagent/events.go:107-137`); rendering *why* something was
allowed or asked is a trust surface no mainstream assistant has. Fourth:
mission/plan visibility — read-only projection of `mission_plan` events, the
way `acpsvc/plan_projection.go` already does it.

**Rejected, with reasons.**
- *Agent-sessions-view integration.* VS Code's third-party agent surface is a
  curated partner list (Anthropic, OpenAI) billed through Copilot; the
  community API request (microsoft/vscode#325827) sits open with no API. This
  is the gated door the strategy already named. Do not build toward it.
- *A `@contenox` chat participant as the primary chat.* It rides Copilot
  Chat's UI and sign-in, and it is unavailable in the forks (Cursor,
  Windsurf, VSCodium) where the non-Copilot audience actually lives. The
  webview chat is the right primary. Note the docs currently promise
  `@contenox` (`docs/integrations/editors/vscode-vscodium.md`,
  `vscode-permission-bridge.md` smoke test) while no participant is
  contributed — fix the docs, not the manifest.
- *Terminal and fs delegation through the bridge.* acpsvc has both
  (`external_terminal.go`, `fileio.go`) because ACP clients own their editors'
  buffers. In VS Code the runtime and extension host share a filesystem; the
  OS-fallback path is already correct. Weeks of attention for symmetry, not
  capability.
- *Reimplementing the old component system.* The 26 vendored files in
  `webview-src/ui/` (pinned at commit `e2a09836`) *are* the hardened
  components — ChatComposer, ChatMessage, the scroll machinery. Reimplementing
  discards proven code to buy nothing; resurrecting the workspace is banned by
  §5 of the brief. When more components are needed, vendor more of the
  closure the same way. The hardening matters and it is already captured.
- *Visual chain editor, fleet board, backend manager in the IDE.* Operator
  surfaces; see §4.
- *Cursor-style inline edit (⌘K) and edit-checkpointing.* Real table stakes
  at funded-team scale, a trap at solo scale: it duplicates the
  approval-diff flow through a second UI channel with its own undo semantics.
  The diff-first approval card is the honest version of this feature.

## 3. Architecture: keep the bespoke bridge, converge the machinery beneath it

Decided: keep `vscodeagent`'s JSON-RPC dialect. The maintainer's instinct is
right and the code supports it. The bridge already earns its keep in exactly
the places ACP's wire shape would fight it: `autocomplete` as a first-class
cancellable RPC (ACP has no FIM concept), `listModels`/`listProviders` with
live catalog reconciliation (`server.go:747-811`), editor-context injection
(`chat.go:895`), and a permission flow that already borrowed ACP's best idea
— `session/request_permission` as a blocking reverse request using `libacp`'s
own types (`client_requests.go:36-42`). Converging the extension onto acpsvc
would buy resume, missions, and plan for free, but would cost the FIM path
and the VS Code-shaped methods, and would put a gated platform's client on
the protocol whose registry is frozen. The dialect is cheap; the machinery
behind it is what must not fork.

Three concrete convergences, in order of importance:

**Stop replacing the bus.** `EffectiveTaskEventSink` has exactly one
production call site — `vscodeagent_cmd.go:277` — and a non-nil sink
*replaces* `BusTaskEventSink` (`enginesvc/engine.go:183-187`). Consequence
chain: vscode runs publish nothing to the bus; and when a late verdict
resumes a suspended run (`hitl.go:456` → `Respond` → resume hook), the
resumed segment runs under a fresh request ID that no turn is registered for,
so `publishTaskEvent` drops every event (`events.go:47-50`) — the run
completes invisibly. Compose the sinks (bus plus bridge, or subscribe the
bridge off the bus per-session the way `acpsvc.translateEvents` does) rather
than substituting. This single change is what makes resume *renderable* at
all.

**Grow the bridge's matrix to full contract coverage and assert it in CI.**
`bridgeEventSink.Wants` covers 9 of 15 kinds (`vscodeagent/events.go:18-36`);
the six drops include the two HITL-critical kinds. acpsvc's translation is
pinned by `engineEventTranslationMatrix` (`acpsvc/events_test.go`) and a
column in `docs/development/engine-events.md`; the bridge has no equivalent.
Add a "vscode" column to the matrix and a mirror test in
`vscodeagent/events_test.go`. Explicit no-op cases (like acpsvc's documented
`chain_suspended` no-op at `events.go:47-49`) are fine — *undocumented* drops
are what the 2026-07-28 memo calls declared-but-unenforced standards.

**Do not extract a shared translator.** Three translators of one stream is
indeed one too many in spirit, but the fix is not a fourth abstraction.
acpsvc's two (`publishEvent`, `nativeEventTranslator.publish`) differ by
lifetime — connection-bound versus turn-journal — and are kept case-identical
by a test, which is the repo's proven mechanism. Extend that mechanism to the
bridge (shared *contract*, per-surface *code*) instead of forcing acpsvc's
libacp-typed helpers into a neutral package. The session memo's lesson
applies: enforcement lives in tests, not in shared code that must then be
maintained for three consumers.

Two smaller architecture calls. The `approve_contenox_tool_call` native tool
is registered and contributed but `requestNativeApproval` has zero call sites
(`nativeTool.ts:29`); either wire it per step 5 of
`docs/development/vscode-permission-bridge.md` (thread `toolInvocationToken`
through the turn) or delete it — a shipped-but-dead approval path is exactly
the "advertised but not enforced" class the memo bans. And the LM-provider
path auto-denies every permission request (`lm/provider.ts:149`): acceptable
as a fail-closed default, but it should be surfaced in the model picker
description ("read-only tools via this path") rather than silent denial, or
routed to the webview handler when the panel is open.

## 4. The eleven admin pages

The old `contenox serve` pages, page by page (`docs/rnd/beam-web.md` for what
they were). "IDE" means the extension; "CLI" means it stays a `contenox`
verb; "dead" means no successor anywhere.

| Page | Verdict | Reason |
|---|---|---|
| backends | CLI | Operator plumbing; the IDE needs only the read-only provider/model pickers it already has (`RuntimeControlsView.ts`). |
| chains | CLI | Chains are versioned JSON validated by `contenox vet`; the dagre editor was legible but is a funded-team luxury. |
| control | IDE — shipped | It is the `contenox.controls` view today. |
| fleet | CLI | Unattended-fleet ops belong where `fleetboot`/missions are driven; an IDE fleet board is an ops console in the wrong window. |
| hitl-policies | CLI (IDE picks) | Policy *editing* is runtime-side by hard boundary; the IDE selects policies (shipped) and renders decisions. |
| inbox | IDE | The walk-away surface. Pending asks as an activity-bar badge is the single highest-leverage page revival; see §7 step 2. |
| missions | IDE (read-only) | Status, plan, ask, report rendering — projection only; dispatch stays `contenox mission` until evidence demands more. |
| models | CLI | Same as backends. |
| prompt | dead | A playground; chat supersedes it. |
| remotehooks | CLI | Registration/config is operator work (`contenox mcp`); the IDE already *lists* MCP servers read-only. |
| settings | IDE — shipped | Split correctly today between VS Code settings (15 keys) and runtime config via `setConfig`. |
| (chat) | IDE — shipped | The webview is its successor. |

## 5. The TUI

Keep beam, feature-frozen. The settled strategy requires an un-gated attended
surface, and beam is it: Microsoft can gate an API, delist an extension, or
fork-block a marketplace; it cannot touch a terminal program installed by
`install.sh`. Retiring beam would make VS Code load-bearing, which §2 of
WORK.md's positioning section forbids and the brief's §3 tension exists to
prevent. Freezing is honest about cost: beam is 13.9k non-test LOC
(`internal/surfaces/beamtui`), but its steady-state tax is low because it
consumes acpsvc's notifications rather than raw engine events (its
`enginebridge` is an in-process ACP client — `bridge.go:330` — with an
`UnknownUpdate` forward-compat arm), and testkit's symbolic goldens survived
a full recolor with zero churn. The honest marginal cost of "both" is that
every new *session-contract* feature lands twice; the mitigation is that
frozen means frozen — beam gets contract updates and bug fixes, and every new
component (palette entries, views, pickers) lands only in the extension.
Revisit only if the extension is delisted or its APIs gate (§6 fallback).

## 6. Distribution

The premise correction matters here: the listing is live —
`contenox.contenox-runtime` v0.36.0, published stable, updated 2026-06-28,
15 installs, no reviews. Distribution is not a decision to make; it is a
funnel to unblock.

**Open VSX, immediately.** There is zero Open VSX presence — no `ovsx` hit
anywhere in CI, and the namespace 404s. In 2026 Open VSX is the registry for
the entire fork ecosystem (Cursor, Windsurf, VSCodium, AWS Kiro, Google
Antigravity; the Eclipse Foundation reports 300M downloads/month and shipped
a managed registry in April). Those forks are precisely the non-Copilot
audience the webview-chat architecture serves best — Copilot Chat and its
model picker don't exist there, so contenox's self-contained panel is at its
strongest. One publish step in `vscode-marketplace.yml`, one namespace claim,
zero code changes. This is also the "un-gated" answer for an extension: the
same VSIX ships to the Marketplace, to Open VSX, and as a GitHub Release
asset for sideloading (already documented in
`docs/integrations/editors/vscode-vscodium.md`).

**Fallback if Microsoft gates the APIs.** Inventory what the extension
actually depends on: webviews, tree views, commands, settings, status bar,
`InlineCompletionItemProvider`, `registerTool`, and two 2025-finalized AI
APIs — `LanguageModelChatProvider` (finalized v1.104) and the MCP server
definition provider. `enabledApiProposals` is absent and CI-enforced
(`assert-package-clean.js`). The worst plausible gating — Copilot-adjacent AI
APIs restricted to partners — costs the model-picker registration and the MCP
provider, both secondary reach; the core product (webview chat, approvals,
completions, code actions) rides APIs that thousands of non-AI extensions
share and that the forks must keep compatible. If the Marketplace itself
turns hostile, Open VSX plus sideload plus beam is the escape hatch, and it
requires nothing built in advance beyond the Open VSX publish step.

**Consolidate the release path while touching it.** The 5-target matrix is
maintained twice (`vscode-marketplace.yml` and the duplicate `vscode` job in
`release.yml:113-230`), both bypass `scripts/package-target.js`, and
`CHANGELOG.md` is stuck at 0.28.14 while the version auto-slaves to
`internal/version/version.txt`. One workflow should own the matrix; the other
should consume its artifacts.

## 7. Sequence

Costs are maintainer-attention, calibrated to this codebase's demonstrated
day-rate (the 2026-07-28 audit remediated ~20 findings in a day with agents).

1. **Walk-away in the IDE** (3–5 days, the only step that is architecture):
   compose the event sinks instead of replacing the bus; add `chainSuspended`
   notification + `sessionResume` RPC to the bridge; register the resumed
   request ID so the resumed segment streams; render suspended state and a
   resume affordance in the webview; extend the engine-events matrix with a
   CI-asserted vscode column. Exit test: park a gated call, close VS Code,
   answer via `contenox approvals respond`, reopen, watch the run finish in
   the transcript.
2. **Inbox surface** (2–3 days): pending-approval badge + tree over the
   existing `operatorinbox` events; answerable cards outside a live turn.
   This is the demo the positioning doc asks for — a boring thing (an
   approval answered late) working on a real outcome.
3. **Open VSX** (half a day): namespace claim, `ovsx publish` job appended to
   the existing publish gate, doc line for the forks.
4. **Polish debt in the webview and bridge** (3–4 days, mechanical): session
   rename end-to-end; `listTools` (currently hardcoded `[]`); one automatic
   bridge respawn with backoff; wire-or-delete `requestNativeApproval`;
   delete dead code (`sessionTitle.ts`, inert `turnInProgress` context keys);
   migrate `RuntimeControlsView`'s inline-HTML webview onto the React
   pipeline or explicitly accept the two-stack split in a comment; fix the
   `@contenox` doc claims; changelog automation.
5. **Mission/plan read-only rendering** (2–3 days, only after 1–4): project
   `mission_plan` tool events the way `plan_projection.go` does; mission
   status in the sessions tree.
6. **Decide the LM-provider stance** (half a day): label it read-only-tools
   or route its permissions to the webview handler.

**Not doing:** terminal/fs delegation in the bridge; chain editor; fleet
board; agent-sessions-view integration; `@contenox` participant; inline-edit
(⌘K) UI; edit checkpoint/undo; any `enabledApiProposals`; win32-arm64/alpine
targets; webview component reimplementation; a second extension.

## 8. What would change my mind

- **Marketplace hostility with teeth:** a policy change or delisting that
  hits binary-bundling agent extensions, or the AI APIs the manifest uses
  moving behind a partner program. Then the extension demotes to the Open
  VSX/sideload channel and beam unfreezes as primary attended surface.
- **The forks converge on ACP.** If Cursor/Windsurf/Zed-class editors make
  ACP client support table stakes and the registry unfreezes, the calculus in
  §3 flips: one acpsvc client shipped as a thin extension beats a bespoke
  dialect, and `vscodeagent` becomes the thing to retire.
- **Install data staying flat after steps 1–3 ship and get the playbook
  treatment.** 15 installs is pre-marketing noise; 15 installs three months
  after walk-away + Open VSX + two proof posts would say non-CLI developers
  are not reachable at solo cost, and attention should return to beam and the
  read-only run page (WORK.md's parked "visible surface" candidate).
- **Dogfood failure:** if the maintainer's own coding sessions keep landing
  in beam because the webview chat fundamentally can't match terminal
  ergonomics, believe the feet, not the strategy doc.

## 9. Unknowns

- **Whether contenox models appear in the picker without Copilot sign-in.**
  `LanguageModelChatProvider` is finalized, but the picker lives in Copilot
  Chat and there are open issues about provider models not appearing
  (microsoft/vscode#277165). Check: fresh profile, no GitHub session, install
  the extension, open the model picker. This decides how much the
  LM-provider path is worth.
- **MCP server definition provider API stability tier.** I believe it
  finalized in mid-2025; I did not verify the exact version or its absence
  from any enterprise policy gate. Check the API docs page and
  `vscode.d.ts` for the engines floor (`^1.96.0` currently declared —
  verify the LM provider API actually exists at 1.96, since it finalized in
  1.104; the manifest may be under-declaring its floor).
- **Journal wiring.** `NewKVJournalTaskEventSink` appears to have no
  production call site (only `events_journal_test.go`), while
  `engine-events.md` §5 describes it as the durable record. If true, replay
  and the behavior-suite plan rest on a sink nothing constructs — verify with
  a call-site grep and either wire it in `enginesvc` or correct the contract
  doc.
- **Open VSX namespace availability and publish friction** (token, namespace
  verification for the `contenox` name). Ten minutes at the console to
  confirm.
- **Whether `chatSend`'s missing turn registry entry is the only reason
  resumed segments are silent.** The analysis chain
  (`vscodeagent_cmd.go:277` → `engine.go:183-187` → `events.go:47-50`) was
  read, not executed. Step 1's exit test is the verification.
- **2026 external facts** cited in §6 (Open VSX scale, fork adoption, VS Code
  third-party-agent gating) came from a same-day web check, not from reading
  primary policy documents end-to-end. The load-bearing ones for sequencing
  are the Open VSX facts; re-verify before citing them publicly.
