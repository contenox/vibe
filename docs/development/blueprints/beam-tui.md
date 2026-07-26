# beam TUI — component blueprint

This is the component plan for contenox beam, the terminal UI that is the product's new face. contenox is an open coding harness — any model, any editor — and beam is where its signature moves live: missions fired-and-detached under an approval envelope, low-ceremony HITL, and a voice that is enjoyable first. The requirements below were refined on 2026-07-26 by a multi-agent pass (12 component requirement sets, a completeness critic, 9 maintainer-identified gap concerns, and an ACP-protocol crosscheck). Decisions live in this document; the code does not exist yet. Paths are written in the post-restructure layout (module `github.com/contenox/beam`): kernel packages under `internal/kernel/`, model plumbing under `internal/models/`, domain services under `internal/services/`, surfaces under `internal/surfaces/` (the TUI itself lands in `internal/surfaces/beamtui/`), and `libacp/` stays where it is.

## 1. Decisions already made

Do not relitigate these; every component spec below assumes them.

- **Full ACP command parity (CORE DESIGN, maintainer 2026-07-27).** beam
  surfaces every ACP slash command — `/mission` included — with zero
  beam-side command reimplementation. This is guaranteed by the in-process
  loopback below (beam drives the real Transport, so command advertisement
  and dispatch are identical to an editor's) and is an acceptance criterion
  for the command-palette component: the palette renders the same
  `available_commands` set an ACP editor receives. Missions are kept and
  first-class; the mission-panel component stands.

- **In-process ACP loopback.** beam consumes `internal/surfaces/acpsvc.Transport` in-process via an in-memory duplex pipe carrying real libacp wire types: one `libacp.AgentSideConnection` wraps the Transport, one `libacp.ClientSideConnection` (beam's own `libacp.Client` impl) sits on the other end. This is the exact two-Run-loops-over-`io.Pipe` pattern already proven end-to-end in `internal/surfaces/acpsvc/client_loopback_test.go`. No subprocess, no socket, no stdio. A side benefit is load-bearing: routing through a real connection pair inherits libacp's `safeCallMethod` panic containment (`libacp/conn.go`) instead of beam owning all recovery itself.
- **Stack.** bubbletea + lipgloss (+ bubbles, teatest). None of these are in go.mod today; their arrival is deliberate and happens with the first beam packages (theme-styles/test-harness), closing the TODO §8 gate.
- **Missions are the signature.** Fire-and-detach a bounded unit of work at a declared agent under an HITL envelope; reports stream back into the firing session. `/mission` (already implemented server-side) is the sole successor of the old `/plan`; the interactive plan-stepping verbs are not resurrected.
- **HITL: one policy, no renagging.** Policy is a named envelope selected up front; every gated call renders one inline card answered with one keystroke. No client-side "always allow" cache the policy engine doesn't know about — the codebase's own invariant (`internal/surfaces/contenoxcli/hitl_tty.go` doc comment) stands unless the maintainer explicitly overturns it (see D19).
- **Inspiration.** The xAI Grok TUI look-and-feel class of coding TUI (also Claude Code): transcript-first, quiet chrome, streaming text, native copy/paste as a headline requirement.
- **Build on services; surfaces stay thin.** Every number, diff, policy evaluation, token count, and session record shown in beam is produced by runtime services; the TUI renders and dispatches, never re-implements. Any behavior a component needs that a service doesn't yet expose is prerequisite service work (section 4), not TUI logic.

## 2. Open decisions for the maintainer

Deduplicated union of every component's open questions. Each item names its claimants. These are the calls a human must make; implementation agents should treat unanswered items as blockers only where marked.

### Rendering & input model

- **D1 (blocks app-shell + transcript-view — the single biggest fork).** Alt-screen virtualized viewport vs inline/native-scrollback rendering with a bounded live region (the Claude Code pattern). Determines whether long-session virtualization is beam's problem or the terminal's, whether collapse/expand can ever apply to already-scrolled-past cards, and the whole copy/paste story. A persistent side/mission panel argues alt-screen; the copy/paste headline argues inline. Both components declared this "the central open question" without cross-referencing each other.
- **D2.** Ratify zero terminal mouse-capture as a hard MVP commitment (selection-clipboard asserts it as a MUST; app-shell and diff-view still treat it as open). If ratified, all pane/scroll interaction is keyboard-only in V1 and any later mouse affordance is opt-in.
- **D3 (blocks keymap-registry policy).** Ctrl+C semantics: quit-immediately (old vibe) vs interrupt-in-flight-turn-then-quit-on-second-press (Claude Code convention, requires the engine-bridge cancel primitive). Also arbitrates the three competing claims on the chord: app-shell global quit, composer clear-non-empty-buffer-first, shell-pane SIGINT-into-PTY — these must be expressed as keymap-registry scopes under one policy.
- **D4.** Submit-vs-newline chord in the composer. Shift+Enter is not reliably distinguishable from Enter without Kitty-protocol/modifyOtherKeys negotiation; pick the primary chord and a documented fallback (Alt+Enter, Ctrl+J, or a line-continuation convention).
- **D5.** Reserved exit chord for shell-pane passthrough focus (candidates like Ctrl+\ collide with SIGQUIT); needs real-terminal testing before lock-in (keymap-registry, shell-pane).
- **D6.** Approval keybindings: reuse the CLI's y/N/q or a beam-native scheme, and does an in-session card offer the CLI's "abort the whole run" option, which reads differently in a persistent TUI? (approval-cards)
- **D7.** Confirm Ctrl+Z suspend stays enabled given detached missions keep running regardless of foreground state (app-shell; one-line sign-off).

### Shell passthrough

- **D8.** Prefix character: `$` (brief, old vibe, Grok/Claude-Code family) vs `!` (the already-implemented backend, its tests, docs, and the retired web UI). Pure TUI-side parsing choice, but creates a naming split with existing docs if `$` wins (composer, shell-pane).
- **D9.** Execution path for the user's line: the warm per-session `internal/services/shellsession` PTY (same cwd/env the agent's shell tool sees; output streams asynchronously) vs a fresh ephemeral exec like old vibe (synchronous, different feel). Decides the display contract with the pane (engine-bridge, composer, shell-pane). The user's own line staying HITL-exempt is existing precedent to ratify, not reopen.
- **D10.** Fate of the ACP wire terminal surface (`extMethodTerminalRun` / `TerminalOutputMetaKey` in `internal/surfaces/acpsvc/terminal.go`) now the web frontend is gone: keep alive for external ACP clients, or let beam's in-process seam supersede it and prune later? (shell-pane)
- **D11.** Any deliberate, user-triggered path for shell output to reach model context (an `@shell` promotion)? Current architecture deliberately has none beyond the agent's own read tool (shell-pane).
- **D12.** Do `$` lines participate in Up/Down history recall? They aren't persisted to the transcript, so yes means composer owns a small local recall buffer (composer).

### Sessions & tabs

- **D13 (blocks app-shell layout).** Session model: single visible session per beam process with in-place `/session switch` (old vibe), or visible multi-session tabs? Transport already hosts many concurrent sessions; this is a UI mental-model call that also decides what the mission badge and notification "active surface" compare against (engine-bridge, app-shell, session-manager, completion-notification).
- **D14.** The active-session KV pointer (`contenox.session.active`) is global per workspace and reassigned by every ACP editor tab's `session/new`. Recommendation on the table: beam reads it once at startup, then tracks its own session in memory and writes back only on explicit switch/new — confirm this divergence is acceptable (session-manager).
- **D15.** ACP-editor-spawned sessions (`zed-<uuid>` etc.) in beam's session list: show, filter, or demote/group with the first-user-message Title convention? (session-manager)
- **D16.** `contenox beam --session <name>` with an unknown name: error-and-suggest (CLI behavior) or auto-create with confirmation (friendlier for a TUI)? (session-manager)
- **D17.** Auto-derive session names from the first user message (à la ACP Title) instead of `session-XXXXXXXX`-until-renamed? New behavior for all surfaces (session-manager).
- **D18.** Is "session deleted by another process → next turn errors, user recovers via /session new" an acceptable failure mode, or must beam re-check existence per turn? (session-manager)

### HITL & approvals

- **D19.** The always-allow affordance: honor the existing no-cache invariant (nothing ships), remap it to a one-keystroke switch to a more permissive named `/policy` preset (coarse, zero new engineering), or invest in a versioned allow-rule write path from the TUI (real service work, needs its own review)? (approval-cards)
- **D20.** Attention-asks (`mission_ask_attention`, free-text Q&A): owned by approval-cards as one unified "beam inbox" (what the repo's own comments in `internal/services/hitlservice/attention.go` assume) or by mission-panel? Must be split explicitly so it isn't built twice — currently claimed by neither (approval-cards, mission-panel, round-1 critic).
- **D21.** Mission-approval queue backfill (`ListPending`): automatically at startup/session-switch (never miss an ask) or only when the operator opens the panel (no clutter from other missions)? (approval-cards)
- **D22.** Operator-inbox (`internal/services/operatorinbox` — reports/asks with no live parent session) has durable data and no UI owner at all: mission-panel later, a separate inbox surface, or explicitly deferred? Without an answer, a mission's report can become permanently invisible (mission-panel, critic).
- **D23.** Should approval/attention notifications bypass focus suppression and always ring, since an unanswered ask blocks a unit until its ceiling expires? (completion-notification)

### Missions

- **D24.** Listing/badge scope: only missions fired from sessions this beam process supervises (`MissionsFiredBy` semantics) or every mission the local in-process fleet dispatched — supervision edge vs fleet board (mission-panel, app-shell).
- **D25.** Stop terminalization: today `fleetservice.Stop` does NOT terminalize the mission record, so the panel must call Stop then `missionservice.Finish(StatusAbandoned)`. Keep that two-call responsibility in the panel, or fix `internal/services/fleetservice` so every future caller benefits? (mission-panel; also listed in section 4)
- **D26.** Expose Cancel (interrupt turn, keep unit) separately from Stop (kill unit), or is Stop the only verb for disposable unattended units? (mission-panel)
- **D27.** Mission monitor: a fired mission is literally another `contenox acp` subprocess with its own unit session (tagged in `_meta`). Is the text report the only visible artifact for MVP, or does beam offer a "view unit session" action — and are unit sessions hidden/relabeled in the ordinary session list either way? (engine-bridge, mission-panel)
- **D28.** Steering: now that /plan's step controls are gone, is fire-and-forget-with-streaming-reports intentionally the full stop on steering a running unit? (command-palette)
- **D29.** Plan updates in plain non-mission sessions: does transcript-view need a fallback plan renderer, or is mission-panel the mandatory host for ALL plan updates? (transcript-view, ACP crosscheck)
- **D30.** Per-mission token budget display: mission-panel or context-budget? Same `usage_update` channel, different surface — named split required so neither assumes the other renders it (context-budget).

### Context recovery

- **D31.** Mechanical trim: new `/trim` command or `/compact --force` flag, and what emergency keep-count/byte-cap is provably safe even when a single message alone exceeds budget? (context-budget)
- **D32.** `StopReasonMaxTokens` today covers both "model finished a long answer" and "wedged history"; is a distinct sub-signal needed or is `Used>=Size` alongside the stop reason sufficient? (context-budget)
- **D33.** Extending the poison-pill guard to per-step results (the actual reproduction of the wedge bug) risks dropping a legitimately large user-requested result: truncate-with-marker vs reject-with-error vs prompt-before-drop. Service-design call that blocks finalizing context-budget's acceptance criteria (context-budget).

### Copy & vision

- **D34.** "Copy last answer" payload: raw markdown source (lossless round-trip, fits "any editor") or rendered plain text? (selection-clipboard)
- **D35.** Also expose copy actions as slash-style text triggers (`/copy`, `/copy block 2`) intercepted client-side, or keybind-only? (selection-clipboard)
- **D36.** True in-terminal binary image paste (iTerm2/Kitty/WezTerm, all non-standard) in V1, or is file-path-based attach the whole MVP? (selection-clipboard)
- **D37.** Image accept/downscale/re-encode policy ownership (composer vs selection-clipboard vs engine-bridge/taskengine) and whether the retired web composer's numbers (png/jpeg/webp/gif/bmp, 1568px long edge, 5MB post-encode, PNG-or-JPEG re-encode) still fit the ACP base64-in-JSON path. Genuinely unowned; blocks the vision front door regardless of who implements it (composer, selection-clipboard, critic).
- **D38.** Build the composer's attach/stage UI now against a stubbed `PromptCapabilities.Image`, or wait for the FlattenContent vision fix (section 4) to land first? (composer)
- **D39.** Should an @-mentioned image file route into the ImagePart pipeline instead of being excluded from completion — and who reads/encodes the bytes? (file-addressing)

### Theme & presentation

- **D40.** Light/dark auto-detection (OSC 11) fallback default when inconclusive (dark, matching vibe?) and whether a manual override ships at MVP rather than later (theme-styles).
- **D41.** Unicode glyphs as baseline with ASCII fallback only in plain mode, or independent legacy-Windows-console detection that degrades glyphs even when color works? Windows is a named first-class target, so this is a maintainer answer, not an implementer guess (theme-styles).
- **D42.** Keep vibe's emerald palette or pick a new visual identity for the relaunch WHY.md frames as a repositioning? Changes the component's first PR (theme-styles).
- **D43.** Does "easy copy/paste" eliminate ALL background/reverse-video chrome, or is background fill fine for never-copied chrome (header/status bars)? Needs a named terminal test matrix (theme-styles).
- **D44.** Tool-call cards collapsed-by-default: fixed, per-tool-kind (reads collapsed, diffs expanded), or a user preference? (transcript-view)
- **D45.** `/help` fully client-side (no round trip, no transcript persistence — avoids a growing help block in every session) vs wire-identical to a plain ACP client (command-palette); relatedly whether `/doctor`'s persistence side effect needs softening.

### Preferences, platform & unassigned seams (from the round-1 critic, unresolved by round 2)

- **D46.** Where do beam-local preferences live (notification toggle+tuning, theme override, keymap file, panel layout, copy-mechanism choice)? The server-side KV config is the wrong home for per-user TUI prefs; candidates: a local dotfile owned by app-shell, or flags/env-only with no persistence in V1 (completion-notification, theme-styles, app-shell, keymap-registry).
- **D47.** Windows parity needs one cross-cutting owner: Ctrl+Z/SIGTSTP has no Windows equivalent, creack/pty vs ConPTY semantics differ for shell-pane, and the tmux DCS passthrough logic is moot on Windows Terminal (critic).
- **D48.** Who owns the shared transient notice/toast primitive at least five components currently invent independently (copy confirmations, attach rejections, "already resolved elsewhere", dispatch errors, no-active-session banner) — a small shared component, or app-shell? (critic)
- **D49.** Non-TTY invocation (piped stdout, CI): fail fast with a clear message — which bootstrap layer owns the check? first-run explicitly assumes someone upstream did it (critic, first-run).
- **D50.** Where do unclassified internal errors surface (loopback marshal error, background-goroutine panic report) for bug-report purposes — a debug log pane, a file, stderr-on-exit? (critic)
- **D51.** Name the composition root: the single place that sequences open DB → resolve session → build engine+transport+fleet → start the tea.Program → hydrate transcript → accept input, and handles partial failure at each step (critic; likely engine-bridge bootstrap + app-shell, but it needs a name and an owner).

### Liveness, notification & testing tuning

- **D52.** Stall threshold: uniform 8s or per-kind (a shell command silent 30s is normal; an LLM step silent 8s is not)? Plus: sub-second timer precision in the first seconds, and whether the frozen-frame telemetry overlay ships as a maintainer regression guard (liveness).
- **D53.** Notification constants sign-off (1.5s burst coalescing, 2s rate floor) and the definition of "active surface" broad enough that every event has a suppression comparison target, including non-session surfaces (completion-notification, session-manager).
- **D54.** Golden files: raw ANSI (exact, unreviewable diffs) vs ANSI-stripped plain text (reviewable, misses style regressions). Must be decided before goldens proliferate (test-harness).
- **D55.** Does test-harness write the first golden/liveness test for each component, or only ship helpers+fixtures+CI wiring with component owners writing their own? Assumed the latter; confirm so no component ships untested (test-harness).
- **D56.** Is the liveness metric exactly "View() bytes differ across ticks while active, stabilize while idle", or stronger (spinner glyphs cycle in order, timer digits monotonic)? (test-harness)

### Engine & turn lifecycle

- **D57.** Thinking/reasoning trace: transcript-view's MVP requires rendering `agent_thought_chunk` distinctly while engine-bridge lists the event slot as an open question — reconcile: in or out of MVP (engine-bridge, transcript-view).
- **D58.** Should an ordinary (non-mission) turn ever survive beam process death (detached engine daemon), or is "closing beam kills your in-flight turn, like closing a terminal" the V1 answer? Missions already survive independently (connection-lifecycle).
- **D59.** Confirm beam never points at a remote engine in V1 (CONTENOX_SERVER_URL forwarding was removed); a yes later reshapes connection-lifecycle significantly (connection-lifecycle).
- **D60.** Is first-run's Ready gate boot-only, with mid-session backend regression (Ollama drops on laptop sleep, key expires) explicitly unowned-for-now — or does connection-lifecycle's Degraded absorb it? Point at one owner (first-run, connection-lifecycle).
- **D61.** May users browse existing transcripts read-only while `Ready()==false`? Recommended yes; session-manager owns enforcing the boundary (first-run).

## 3. Service extensions required first

Per the build-on-services rule, these land in kernel/services/surface-service packages before (or alongside) the TUI components that need them. Grouped by owner.

### libacp + internal/surfaces/acpsvc

1. **Vision front door — FlattenContent image drop** (needed by composer, engine-bridge, selection-clipboard, file-addressing later). `libacp/flatten.go`'s `FlattenContent` is a documented lossy text projection that silently drops image/audio/resource blocks; `internal/surfaces/acpsvc/prompt.go`'s nativeDriver flattens every prompt through it, so a correctly-formed image ContentBlock never reaches `taskengine.Message.Images` (`internal/kernel/taskengine/tasktype.go`'s `ImagePart{Data,MimeType}` exists with no producer). The native driver (or agentservice) must preserve image blocks into `Message.Images`. Until this lands, all attach UI is inert; `PromptCapabilities.Image` (hardcoded false in `internal/surfaces/acpsvc/initialize.go`) must also become per-session/model truth for the composer's gate.
2. **Context-overflow stop reason survives the wire** (context-budget — the single highest-leverage fix in this list). In `libacp/conn.go` (~MethodSessionPrompt handling), a turn failing with `taskengine.ErrContextLengthExceeded` must resolve as `PromptResponse{StopReason: StopReasonMaxTokens}` with a nil RPC error, mirroring the existing cancel special case. Today the computed stop reason is discarded and every client sees a generic internal error, forcing forbidden string-matching in the surface layer.
3. **Re-emit `available_commands_update` on capability change** (command-palette, later-tier). The menu is sent only at session bind; `/model` unlocking `/mission` mid-session leaves the palette stale.
4. **Injectable clock/id source** (test-harness, flagged not assumed): only if real wall-clock timestamps/random ids in emitted events make byte-exact goldens impossible.

### internal/surfaces/contenoxcli → shared packages

5. **Extract `buildInProcessFleet`** (engine-bridge prerequisite, req 9 — real, not nice-to-have). The ~100-line fleet composition (agentregistry + agentinstance kernel + operatorinbox + reportrouter + fleetservice) is unexported inside `internal/surfaces/contenoxcli/acp_cmd.go`; beam and `contenox acp` must share one exported constructor.
6. **Extract an onboarding/apply package** (first-run). `registerSetupBackend`, the `setupProviders` menu table (its own DRIFT-HAZARD comment says it should derive from `providerservice.ListSupportedProviders`), and runSetup's KV-persist tail must become importable by both CLI and beam. `internal/setupcheck`'s package doc promises "no I/O", so this needs a new home (e.g. `internal/onboarding`) or an explicit contract change — location is a maintainer call.
7. **Ollama auto-registration** (first-run zero-config path): a mutating sibling to `setupcheck.EnrichResultWithOllamaProbe` that actually registers the probed local backend; plus confirm the engine build path runs unconditionally on a virgin install (BuildEngine already tolerates empty defaults).
8. **Extract the diff engine** (diff-view). The only diff implementation in the repo is the private LCS in `internal/services/localtools/hitl.go` (`unifiedDiff`/`lcsEditScript`); extract to a shared package or take a small dependency — do not create a third implementation.
9. **Extract the gitignore/skip-dirs matcher** (file-addressing). `internal/services/localtools/fs_gitignore.go` + `fs_policy.go`'s `defaultSkipDirNames` must be reusable so @-mention completion and the agent's own `find_files` filter identically; where it lands (shared package vs folded into vfs/localfileservice) is open.

### internal/services/chatservice

10. **Mechanical `TrimHistory`** (context-budget — the guaranteed recovery path). `/compact` runs an LLM chain over the very history that is over budget, so it cannot rescue a wedged session; add a non-LLM trim (drop oldest/oversized without a model call) and wire it as an acpsvc command (`/trim` or `/compact --force`, D31) alongside `handleClear`/`handleCompact` in `internal/surfaces/acpsvc/commands_session.go`.

### internal/services/agentservice

11. **Poison-pill guard for per-step results** (context-budget — the actual reproduction of the 19,734/19,727 wedge). The current guard only skips persisting when overflow hits before any real step (`len(stateUnits)<=1`); an oversized mid-turn tool result is persisted anyway and poisons the session. Policy for the fix is D33.
12. **Persistence outcome signal** (connection-lifecycle). `persistHistory` swallows commit failures (reported only to the tracker); `Prompt` returns success even when the exchange was not durably saved. The caller needs a `persisted: false` signal so Degraded's persistence counter and a user warning are possible.

### internal/kernel/taskengine

13. **`TaskEventHistoryShifted` event kind** (context-budget, later-tier transcript note). `shiftMessagesToFit` drops messages silently as far as clients are concerned (`token_usage_post_shift` reaches only the tracker); a client-visible event enables "dropped N oldest messages" notes without client-side inference.

### internal/services/mcpworker

14. **Worker health getter** (connection-lifecycle). `listToolsCooldownUntil`/`listToolsLastError` are unexported and `ActiveWorkers()` returns only names; add `Status(name)`/`Health()` so Degraded can name the down server before a tool call fails mid-turn.

### internal/kernel/nativeturn

15. **recover() in `turnSession.run`** (upstream hygiene, not beam-blocking — beam does not wire `Deps.NativeTurns`/`Instances` in V1). `go ts.run(fn)` has no recover, so a chain panic there kills any process that does wire the registry.

### internal/services/fleetservice

16. **Stop should terminalize the mission record** (mission-panel, decision D25). Today Stop leaves a dead unit "open" forever unless every caller remembers the follow-up `missionservice.Finish`.

## 4. Component catalog

All 21 components. "Consumes/emits" are compressed to the load-bearing seams; keybinding specifics defer to keymap-registry; styling defers to theme-styles; every service call goes through engine-bridge unless noted.

### 4.1 engine-bridge

The seam between beam and the runtime: owns the one warm in-process `enginesvc.Engine` and `acpsvc.Transport` for the whole process, submits prompts, translates the resulting notification stream into typed UI events, surfaces HITL permission requests, and dispatches `/mission` through the in-process fleet exactly as an ACP editor does. Zero UI-toolkit imports; a future CLI verb could depend on it.

MVP requirements:
1. Exactly one Engine and one Transport per process lifetime; construction mirrors `internal/surfaces/contenoxcli/acp_cmd.go` (db → bus → hitl → tools → engine → transport); `Engine.Stop()`/`Transport.Close()` run once on exit.
2. Production wiring mirrors `internal/surfaces/acpsvc/client_loopback_test.go`'s io.Pipe loopback (decision made, section 1).
3. Its `libacp.Client` declares FS read/write capabilities false and no terminal capability, so `internal/surfaces/acpsvc/fileio.go`'s ACPFileIO falls back to direct OS file IO — beam implements none of those callbacks for MVP.
4. Session lifecycle calls map 1:1 onto Transport methods (New/Load/Resume/List/Close/DeleteSession); no re-implementation of session bookkeeping, cwd resolution, or MCP registration (`internal/surfaces/acpsvc/session.go` owns those, warm per session).
5. Async `SubmitPrompt(sessionID, text)` returns immediately; results arrive on the per-session channel. `Cancel(sessionID)` maps to Transport.Cancel; a genuine cancel resolves `StopReasonCancelled`, never an error.
6. Every `SessionNotification` kind becomes exactly one typed UI event, delivered in wire order on a single per-session channel — no reordering, cross-kind coalescing, or silent drops.
7. `RequestPermission` blocks only the affected tool-call card; resolves to allow/deny from the user's keystroke; pending requests resolve `PermissionOutcomeCancelled` on teardown — no hung goroutines.
8. Slash commands go through SubmitPrompt as plain text (acpsvc's `parseCommand` intercepts); engine-bridge only surfaces AvailableCommands for autocomplete.
9. Bootstraps the in-process mission fleet (via the extracted constructor, section 3 item 5) into `Deps.Fleet`/`Deps.Agents`; mission reports deliver via `Transport.DeliverToContenoxSession` byte-identically to the editor path.
10. Closing a session cancels its in-flight prompt, unsubscribes feeds, calls CloseSession; leaks nothing (PTY, bus subscription, dispatched subprocess) past exit.
11. HITL policy is a startup default (embedded presets from `internal/surfaces/contenoxcli/hitl_policies.go` or override) fed into Deps; never re-asked per call; no client-side allow memory.
12. Package has zero bubbletea/lipgloss imports, lint-enforced.

Contracts — consumes: `acpsvc.Transport`/Deps, `enginesvc.Engine`, hitl/fleet/mission/shellsession via Deps, libacp types. Emits: session lifecycle results; the ordered per-session typed event stream (text deltas, tool-card open/update/close, plan, usage, config, commands menu); permission-request events; mission confirmations/reports; cancel acknowledgements (cancelled vs failed, distinguishable).

Later: image attachments on SubmitPrompt (inert until section 3 item 1), session replay polish, thinking-stream slot (D57), loopback marshal-cost revisit only if measured.

Open questions (beyond D3, D8/D9, D13, D27, D57): none residual.

### 4.2 connection-lifecycle

Single source of truth for beam's session-health state machine. "Connection" means the health of the in-process engine and its subsystems (MCP subprocesses, backend reachability, SQLite), not a socket. Decides which failures are global versus one turn's inline outcome.

Acceptance criteria (keep):
- States are exactly Initializing (with Resuming sub-phase), SetupRequired, Ready, Degraded, EngineDown. A single turn's error is never a global state.
- A chain step failing mid-turn resolves that one turn (rendered inline) and leaves global state untouched; connection-lifecycle owns only the escalation counting.
- Any panic recovered from an engine-bridge call transitions immediately to EngineDown — engine state after a panic is untrusted. (The loopback decision means libacp's `safeCallMethod` catches handler panics; beam's own recover() wrappers stay as cheap insurance.)
- Degraded enters on: MCP server in failure cooldown; ≥3 consecutive same-provider turn failures in 5 minutes; ≥2 persistence failures. Degraded never blocks prompts; it adds a badge plus one specific recovery action.
- SetupRequired iff Engine nil or `setupcheck.Result.Ready()==false`; recovery is the one existing wizard via terminal suspend (`tea.ExecProcess`/ReleaseTerminal) — never reimplemented as bubbletea views in MVP; on success a fresh Transport is constructed, no hot-patching.
- Relaunch after non-clean exit shows history exactly as of the last fully-returned turn — no ghost messages; the lost-exchange gap is documented, not papered over.
- Resuming runs fixed order: engine build/SetupCheck → DB probe → session load → still-running-missions query (handed to mission-panel); stops at first failure in the matching state.
- Every state exposes ≥1 labeled single-keypress recovery action; EngineDown's is "quit beam" (no in-place rebuild in MVP) and it is the only state allowed a modal/blocking treatment.

MVP: the 5-state machine as one owned type; recover() at every engine-bridge call boundary; do NOT wire `Deps.NativeTurns`/`Instances` (they exist to survive a separate server's client disconnecting — beam is both); Degraded counters reset on next success; document the mid-turn-crash data loss.

Contracts — consumes: engine-bridge call results/errors and panic signal, SetupCheck/SetupStatus, `InferStopReason` classification, mcpworker health (blocked on section 3 item 14), mission still-running query, session-manager's session id. Emits: status value + cause for the status bar; recovery-action list; "interrupted turn, nothing to replay" fact for transcript-view; mission reattach trigger; terminal-suspend request around the wizard.

Later: in-place engine rebuild; eager pre-turn prompt persistence; proactive MCP health; cooldown countdown/manual retry.

Open questions beyond D58–D60: where exactly the suspend/resume hook lives (app-shell owns the Program; needs a definite owner so it isn't duplicated).

### 4.3 first-run

Owns the first frame when the runtime isn't chat-ready — and the whole path to a usable composer: a zero-keystroke path when local Ollama already serves a pulled model, an inline wizard otherwise, teaching-quality error rendering in between. Owns the single Ready gate every other component defers to. Per the ACP crosscheck, this is also contenox's actual auth model: libacp Authenticate is a one-time setup wizard, Logout is permanently MethodNotFound — no login/re-auth UI exists or should.

Acceptance criteria (compressed): fresh dir + no Ollama → wizard on first frame (no blank screen, no nil-engine panic), same provider set/order as `contenox setup`; fresh dir + Ollama serving a chat model → auto-register, set defaults, land in composer with zero keystrokes and one dismissible confirmation line (only Ollama, only when BackendCount==0 and no defaults set); Ollama up but no models → one blocking issue byte-identical to `contenox doctor` output plus a recheck key that flips to ready without restart; broken-backend issue text verified identical to `doctor --json`/ACP `/doctor`; a grep gate proves no other beam package imports `internal/setupcheck` to re-derive readiness; wizard-vs-`contenox setup` KV-row equivalence is test-diffed; the silent path never touches an install with existing backends; `FixPath` (web-only) never renders — `CLICommand` renders as copy-ready inline code.

MVP: gate before app-shell mounts the session UI (Ready true → no interstitial); zero-config probe path (reuses `setupcheck.ProbeLocalOllamaAPI` resolution); inline wizard mirroring `setupProviders` (masked secrets, "found in environment" pre-fill, Vertex ADC hint verbatim); submit performs exactly runSetup's non-interactive core then recomputes SetupStatus, rendering remaining issues from literal Issue text (no paraphrase — zero drift from doctor); manual recheck key; never executes remediation commands on the user's behalf; no isatty check of its own (D49); tone differs for virgin vs broken-but-configured installs.

Contracts — consumes: Engine SetupCheck/SetupStatus via engine-bridge, `internal/setupcheck` shapes, the extracted onboarding package (section 3 item 6). Emits: the single Ready gate (bool + Result) read by composer/palette/mission-panel/session-manager/approval-cards; a setup-completed transition; the zero-config confirmation line.

Later: full backend-diagnostics view post-setup; CanVision surfacing; interval/focus-regain auto-recheck; copy-command keybinding.

Open questions beyond D60–D61 and the silent-auto-connect call (D — covered in section 2 as part of first-run's group; treat "silent vs one-keypress connect" as its own sign-off): masked-input widget — own throwaway `bubbles.textinput` now, migrate to a shared primitive later (also the third ACP-unowned item, section 6).

### 4.4 app-shell

The bubbletea program root: computes and hands out rectangles (transcript top-flexible, composer bottom-fixed 1–6 lines, one-line status bar, optional right mission panel that never reserves empty space), tracks the focus enum, intercepts a short global key set, handles resize/suspend/quit, renders the status bar from state others publish. Deliberately thin — it renders no chat message, diff, plan, or menu.

MVP requirements (compressed):
1. The four regions above; panel claims width only when it has content or is explicitly opened.
2. Recompute geometry on every WindowSizeMsg; no negative/below-minimum region; below ~60x15 render a single "terminal too small" message instead of a garbled layout (vibe's relayout lesson).
3. Exactly one focused region (explicit enum); Tab/Shift+Tab cycles; keys go only to the focused region after global interception; non-key messages go to every mounted region (a detached mission's report lands while typing).
4. Short fixed global set: quit (semantics D3), suspend, focus cycle, PgUp/PgDn transcript scroll regardless of focus, panel toggle. (Post round 2: expressed as keymap-registry `global` scope, not raw literals.)
5. Modal claim contract: a claiming region receives every key exclusively (global interception suspended) until release — the seam approvals rely on. (Reconciled with keymap-registry's focus push/pop: app-shell hosts composition; the registry arbitrates keys.)
6. Status bar segments, all sourced from publishers: session name, model+provider, context budget (only after the first `TaskEventTokenUsage` — never a misleading 0/0), mission badge only when count ≥1, health indicator with the closed vocabulary {ready, working, reconnecting, error, disconnected, setup_required} (feeding from connection-lifecycle's states). Always exactly one line.
7. When width can't fit segments, drop whole segments in a documented priority order (proposal: session name first; keep model/provider and health longest) — never wrap or mid-truncate.
8. Every quit path runs a bounded (~2s) shutdown hook then quits regardless — quitting can never hang the terminal.
9. Ctrl+Z suspends where supported (no-op elsewhere); resume forces full clear-and-redraw; suspend never pauses detached mission work.
10. tea.Program construction treats native copy/paste as first-class: no mouse-motion tracking that hijacks click-drag selection (interacts with D1/D2); enables `tea.WithReportFocus` for completion-notification.
11. Global keys, region boundaries, and segment priorities expressed as data, not scattered literals.

Contracts — consumes: bubbletea runtime, mounted children, published status fields, token-usage stream, mission count, modal/focus signals. Emits: per-region geometry, focus-change notifications, forwarded keys, shutdown-hook invocation, tea.Quit, modal arbitration. Also owns (per critic): panic recovery restoring the terminal (cooked mode, cursor, alt-screen exit) on unhandled panic.

Later: rebindable keymap surface, draggable divider, multi-session tabs (D13), configurable segments, plain-output accessibility mode, persisted layout.

Open questions beyond D1–D3, D7, D13: panel toggle key (defers to keymap-registry).

### 4.5 keymap-registry

The single arbiter for every keybinding and owner of the focus/navigation model. No component binds key input directly: bindings are declared ({ID, Keys, Scope, Help, Owner}), collisions fail at build/test time, raw KeyMsg becomes semantic Actions routed to the focused component, Tab cycling and modal focus-trapping live here, Escape walks a fixed priority stack, and the help overlay is generated 100% from registrations. Direct fix for vibe's scattered `switch msg.String()` anti-pattern.

Acceptance criteria (compressed): a Go test fails on chord collisions within simultaneously-reachable scopes (wired into the `-short` CI gate); every binding has Owner and Help.Short and the help overlay contains zero hardcoded strings; a grep gate forbids raw key-literal switches outside the keymap package; focus cycling is a closed loop under test; while any modal is open only its scope is live (one test per modal type); Esc closes exactly the topmost modal per press; Esc with no modal and a turn in flight fires the engine-bridge cancel exactly once and never quits; `?` opens the pure-data help overlay from every scope except shell passthrough; the vim-ish + arrow vocabulary (j/k, h/l, gg/G, ctrl+u/d, Enter, Esc, Tab) is reserved consistently — no pane remaps movement keys.

MVP: Binding type + Register API declared at component init; collision detection (panic in dev, test failure in CI, naming both owners); scopes = global + one per focusable pane + a modal class; focus manager with push/pop for modals; fixed-priority Escape stack (priorities themselves collision-checked); help overlay content model (chrome composited by app-shell — contract to confirm); shell-pane passthrough suspension with one reserved exit chord (D5); reserved non-overridable globals ctrl+c and `?`; gg/G via double-press detection only (no general chord framework).

Contracts — consumes: raw KeyMsg, binding declarations from every component, modal focus-capture requests, turn-in-flight state from engine-bridge. Emits: semantic Actions, FocusChanged (theme-styles renders the ring), help content model, registration-time collision failures. Service extensions: none — cancel already exists (`Transport.Cancel`).

Later: user-remappable keymap file (IDs are stable for exactly this), file-tree scope, general leader-key mechanism, per-terminal capability remapping.

Open questions beyond D3, D5, D46: dynamic vs compile-time Escape-priority registration (leaning dynamic with fixed priority constants, pending file-addressing's popup needs).

### 4.6 theme-styles

The only package allowed to construct lipgloss colors, styles, borders, spacing. Semantic roles (user/assistant/shell/tool/error/muted/border/active/inactive/pending/done/failed/skipped/hitl), chrome constructors (header, status bar idle/working, bordered panel, inline prefix), one status→style mapping function, and a process-lifetime terminal-capability snapshot. Its job is negative as much as positive: no other package can invent an ad hoc color.

MVP (compressed): role lookup is the only path to color (no literal `lipgloss.Color` elsewhere — later a lint guard, à la the deleted web designTokenGuard); named chrome constructors cover every MVP panel kind; exactly one status→style switch for mission/step/approval states; capability detection once at startup (color profile, NO_COLOR, non-tty) with any OSC 11 background query strictly timeboxed — startup must never hang in tmux/CI; adaptive light/dark values, documented fallback default (D40); plain mode preserves meaning via prefix glyphs/labels — color is never the only carrier of meaning; ASCII-safe fallback for every glyph (Windows first-class, D41); copy cleanliness rule: selectable text gets foreground-only styling, background fills reserved for never-copied chrome (D43); named finite spacing/border constants; the go.mod introduction of lipgloss is a deliberate visible change.

Contracts — consumes: env/terminal signals only. Emits: the role/constructor API and the capability snapshot (other components use it for non-color layout decisions, e.g. spinner vs "…").

Later: user theme override, high-contrast/colorblind variants, golden ANSI matrix in CI, lint guard, configurable accent.

Open questions beyond D40–D43, D46: do width/truncation helpers (rune-safe — vibe's multibyte-slice panic is the cautionary tale) live here or in a layout helper.

### 4.7 liveness

Owns "does the user believe something is happening right now": the shared activity-pulse primitive (spinner cadence, elapsed timers, stall detection) every activity-bearing view renders from. Root-caused from the predecessor: vibe's `waiting bool` swapped one static glyph at start and end — no spinner, no tick, no clock — measured at **82–87% frozen frames, one 48.6s still frame**.

Acceptance criteria (maintainer's metrics, keep verbatim):
- **Micro-motion band above 50% during an active turn**: sampling rendered frames at the tick rate while any activity is open, more than half must differ from the immediately preceding frame.
- **No freeze exceeding 1s while a step runs**: the interval between two consecutive distinct frames while activity is open never exceeds 1000ms, regardless of engine silence.
- Ticking is client-side (`tea.Tick`), independent of event arrival: a 10s-silent engine still yields >10 distinct frames.
- Ticker runs only while ≥1 activity is open; at idle, zero ticks scheduled — flat CPU at rest; never fake progress when nothing is happening.
- Stall surfaced honestly: no event for the threshold (default 8s, D52) flips text to "still working — no update for Ns"; elapsed is real wall-clock, never a fabricated percentage.
- Turn end (any StopReason) and mission terminal states freeze the final frame immediately (total elapsed, spinner removed).
- Concurrent activities (mission + turn + shell) get independent elapsed/stall state.

MVP: `ActivityState{Kind: turn|tool_call|mission|shell, ID, StartedAt, LastEventAt, Open}` opened/closed/bumped purely from engine-bridge's translated stream (never engine internals); one shared `activityclock` sub-model owns the tick loop (120–150ms while open, self-disarming); two render-time outputs per activity (spinner frame index, elapsed string) that consumers pull instead of tracking their own clocks; distinct stall phrasing (same spinner, different text); the app-shell aggregate line (count + oldest activity's text — app-shell does layout); ticking continues during token streaming (both motions are true); pure functions over (now, ActivityState) with injected clock so all of it tests without a terminal.

Contracts — consumes: engine-bridge translated events (step/tool/turn lifecycle), StopReason close signals, mission events via engine-bridge, shell-pane start/exit, a clock. Emits: per-activity snapshots (spinner index, elapsed, stalled, text), the aggregate for the status line, a stall-crossed transition others can subscribe to. Service extensions: **none for MVP** (existing event vocabulary suffices; a future fractional-progress event kind is explicit taskengine work, never TUI estimation).

Later: real per-step progress from new engine events, reduced-motion mode, stall/needs-approval notification hook, frozen-frame telemetry overlay (D52).

### 4.8 session-manager

beam's layer over sessionservice/chatservice giving the coding-tool notion of "conversation": resume last active session on startup, replay its transcript, list/name/create/switch sessions in-TUI. Because these are shared un-namespaced tables (one SQLite file, WAL, identity `local-user`), it also reconciles beam's semantics with `contenox chat`'s and the ACP editor's shared active-session pointer — never hijacking another surface's session.

MVP (compressed):
1. Startup: `sessionservice.EnsureDefault("local-user")` exactly as `contenox chat`; `--session <name>` calls Switch (auto-create question D16).
2. Load full transcript via `chatservice.Manager.ListMessages` before accepting input; render user/assistant turns with non-empty content.
3. Always-visible session indicator in the status line (name or short id + message count).
4. `/session list|new|switch|delete [name]` as thin wrappers over `sessionservice.Service` — no new persistence logic; deleting the active session is clearly announced ("no active session" banner, chat blocked until new/switch).
5. `/session rename` wraps `chatservice.Manager.RenameSession` (in MVP; one-line wrapper serving the naming requirement).
6. Same identity constant and `ResolveWorkspaceID` as the CLI so sessions are mutually visible across beam/CLI/ACP — no beam-specific namespace, ever.
7. Switch is a full reload: persist pending diff, clear in-memory history, ListMessages, re-render; tear down everything bound to the old id (in-flight prompt, subscriptions, pending HITL) — a teardown contract with engine-bridge. Also owns gating the update stream by active-session-id on switch (the `FilterSessionUpdates` semantics, per crosscheck).
8. `PersistDiff` after every completed turn (crash loses at most the in-flight turn).
9. Resumed transcripts preserve and re-render existing images (round-trips already tested in chatservice).
10. Never writes messagestore/KV directly — services only.

Contracts — consumes: sessionservice, chatservice.Manager, DB manager opened once at startup, `taskengine.Message` shapes. Emits: resolved session id + hydrated transcript to transcript-view and engine-bridge; switch/new/delete lifecycle events; status-line fields; the session/tab-to-surface mapping completion-notification's suppression check needs.

Later: real picker overlay, `/session fork` + `/compact` reusing existing helpers verbatim, cross-workspace browsing, per-frontend last-used pointer (D14 fork), ACP-session labeling via the existing Title truncation, live multi-writer refresh.

Open questions: covered by D14–D18.

### 4.9 transcript-view

The scrollback pane: the ordered, append-mostly record of one session — user turns, streamed assistant prose with markdown/code rendering, tool-call cards, asynchronously-arriving mission reports — rendered so reading, scrolling mid-stream, and copying out all feel native. A pure renderer over engine-bridge's stream; it calls no service and sends nothing.

MVP (compressed):
1. Typed entries — user, assistant (streaming/final), tool-call card, mission-report card, system/error notice — each distinguishable at a glance.
2. Streaming keyed by MessageID, never "whatever is at the bottom": a mission report can arrive mid-turn with its own MessageID (`mission-report-<id>`, built in `internal/services/reportrouter`) and must render as its own card, never splice into the live stream it races.
3. Throttled redraw during streaming: only the growing message re-renders per frame; type-ahead stays responsive.
4. Markdown (headings, emphasis, inline code, lists, blockquotes) degrading to legible plain text on ambiguity; fenced code blocks syntax-highlighted by language tag, always monospaced, never re-flowed in a way that breaks copy-out.
5. Tool-call cards collapsed by default: one line with ToolKind icon, short title (acpsvc's `toolCallTitle` shape), status glyph; updated in place by ToolCallID through pending → in_progress → completed/failed — never re-appended per transition; expandable in the live region to raw input/output or a diff body (delegated to diff-view).
6. Mission reports recognized by `_meta["contenox.missionReport"]` and rendered as distinctly-styled cards with agent name + report kind (progress/finding/blocker/result) always visible — plain-assistant rendering of a report is a defect, because reports are how detached work talks back into a session the user may not be watching.
7. Thought chunks visually distinct and less prominent than answer text (scope: D57).
8. Auto-follow pinned to the live edge; once the user scrolls up, new arrivals never steal the position — an "N new" indicator plus jump-to-latest instead. A fresh prompt snaps back to live unless mid-scroll.
9. Scroll controls work regardless of composer focus (line/half/full page, top, latest).
10. Resize re-wraps affected content and anchors by message identity, not line offset; re-wrap/re-highlight scoped to changed content so long sessions don't degrade.
11. System notices (cancelled/failed/disconnected) render inline as unmistakably-non-assistant markers; StopReason annotated at turn end.
12. Renders the "interrupted turn, nothing to replay" fact from connection-lifecycle as no dangling entry.
13. Already-settled lines never visibly re-flow as the growing line streams.

Contracts — consumes: engine-bridge's normalized stream (never ACP wire types directly), MessageID/ToolCallID identity, mission-report meta, persisted history from session-manager, geometry from app-shell, fold-state and ScrollToItem calls from transcript-navigation. Emits: scroll state (at-edge, unread count) for the status bar; `Items()`, `LineRange(itemID)`, `ScrollToItem(itemID, placement)` — the navigation seam.

Later: full-turn folding, fast cold-open path for huge sessions, inline images (gated on the vision front door), plan-snapshot rendering (D29), OSC 8 links, side-by-side diffs, copy-affordance fallbacks, theme-aware highlight themes.

Open questions beyond D1, D29, D44, D57: numeric perf bar for "long session" (or accept test-harness's 2,000-item target from transcript-navigation as the shared bar); approval-card hosting boundary (resolved in section 5: approval-cards owns the card, transcript-view provides the inline slot).

### 4.10 transcript-navigation

A pure client-side index + cursor/fold-state layer over the item stream transcript-view renders: jump next/prev tool call and user message, incremental search with highlighting, fold/unfold. Renders nothing itself; never touches the clipboard. No service work needed: session/load already replays full history (the nativeturn journal cap bounds only live-turn replay).

Acceptance criteria (compressed): tool-call jumps collapse a call's status updates to one navigable stop; user-message jumps skip everything between MessageID boundaries; ends are non-wrapping no-ops with a status note; search is literal case-insensitive substring over underlying text (content, titles, rawInput/rawOutput, diff old/new) — never the styled buffer, so lipgloss can't hide a match; a match inside a folded item auto-unfolds exactly that item; sub-16ms query updates up to at least 2,000 items without full rescans per keystroke; fold state is a per-item boolean this component owns and transcript-view only reads; live appends never shift the focused item or selected match; read-only seam for selection-clipboard; zero code added under acpsvc/nativeturn/libacp.

MVP: incremental item index {itemID, kind, MessageID/ToolCallID, status}; the two jump pairs; a minimal search input owned here (never the composer — a query must not be mistakable for a prompt); fold toggle + fold-all/unfold-all via command-palette; `CurrentPosition()`/`SearchStatus()` for the status bar ("tool call 7/23", "match 2/9"); the documented seam with transcript-view (Items/LineRange/ScrollToItem); explicit non-goal: searching embedded terminal buffers (transcript carries only a TerminalID pointer — shell-pane's concern).

Later: regex/fuzzy modes, wraparound with notice, filtered targets (next failed call, next diff), bookmarks, fold-state persistence via the existing per-session KV prefix if ever server-side (D — see fold question), cross-session search (session-manager surface, needs a service capability that doesn't exist).

Open questions beyond D-group 14 (fold persistence local vs server, raw-JSON vs rendered search corpus, mission-origin calls in jumps): fold auto-collapse threshold ownership (transcript-view render-time vs navigation policy — pin jointly).

### 4.11 composer

The single place the user types: multiline buffer with history recall, slash triggering, shell-prefix classification, an explicit submit-vs-newline distinction, and (the vision front door) image staging. It classifies and packages; it never executes shell, dispatches commands, or calls the model. Ownership ruling (resolving the round-1 duplicate-claim): **composer classifies the prefixed line; shell-pane only receives the classified line + text.**

MVP (compressed):
1. Multiline editing table stakes (word-jump, kill-line/word).
2. Enter submits non-empty non-picker buffer; a reliably-detected chord inserts newline (D4); whitespace-only submit is a no-op.
3. Bracketed paste: a multi-line paste is one literal insertion — never multiple submissions, never reinterpreted as slash/shell triggers.
4. Submit clears immediately; failed validation restores buffer and staged attachments.
5. Slash trigger only when `/` is the first non-whitespace char of the whole buffer (mirrors `parseCommand`); opens the palette from the session's advertised AvailableCommands only; selection inserts without submitting; a recognized command submits through the identical path as a normal prompt — no local validation or execution.
6. Shell-prefix (D8) checked before the slash check; classify and hand off only.
7. Up/Down history recall at buffer edges with standard shell semantics (draft restored past newest); seeded from the session's persisted user-role turns (slash commands are already persisted as user turns) — no bespoke store.
8. Image staging behind `PromptCapabilities.Image` (false today — see D38): pasted payload decoded/validated/downscaled per the accept policy (D37), staged as removable chips, bundled as ordered image ContentBlocks on next submit; capability absent → no affordance, paste rejected with a single-line auto-dismissing notice — never silent, never modal; image-only messages are legal.
9. Ctrl+C on non-empty buffer clears first; only an empty buffer bubbles quit-intent (subject to D3).
10. Exposes the `@`-token span + text-splice contract to file-addressing (composer stays the keystroke/textarea owner).

Contracts — consumes: the Prompt path via engine-bridge, AvailableCommands, capability flags, persisted transcript (read-only), libacp content constructors. Emits: outgoing payload (text + ordered images + text/command/shell classification tag), draft/attachment state, history queries.

Later: file-path/`/attach` fallback, OS-clipboard image paste per-platform probe, `$EDITOR` composition reusing `internal/surfaces/contenoxcli/editor.go` wholesale, reordering/captions/audio, Ctrl+R reverse search, fuzzy picker, configurable submit keymap.

Open questions: covered by D4, D8/D9, D12, D37–D38; plus the modal-overlap boundary (buffer stays untouched under an approval overlay — recommended, needs the explicit contract with approval-cards).

### 4.12 file-addressing

Owns naming a file to the agent: detecting a completed `@` trigger, sourcing/ranking candidates from the session's actual grant-bounded workspace root, a live context tray of the draft's mentions, and translating the mention set into the exact ContentBlock wire shape (text + resource_link) acpsvc already pins in tests. Explicitly does NOT re-implement containment, gitignore filtering, or root resolution — it rides vfs/localfileservice/acpsvc.

Acceptance criteria (compressed): `@` triggers only at start-of-input or after whitespace (`user@host` doesn't); candidates come only from the session's resolved workspace root via the vfs allowlist — never the raw process cwd; no root → fixed empty state; noise filtering uses the identical matcher as the agent's own `find_files` (section 3 item 9) so human and agent lists never disagree; control-plane-denied paths and out-of-root symlinks never surface (inherited by walking through vfs, not `os.ReadDir`); ranked (basename-prefix, path-substring, fuzzy subsequence), capped at 20 with a "+N more" footer; ≤150ms debounced, cancellable, never stale-out-of-order; files-only non-image-only in MVP; selection splices `@relative/path ` and registers a tray chip; tray = distinct mentions of the current unsent draft, clears on submit; chip and text token stay bidirectionally in sync; on submit the wire shape is byte-for-byte the pinned contract (one text block + one `ContentKindResourceLink` per distinct path, first-seen order, no embedded content — `internal/surfaces/acpsvc/content_test.go` must not regress); deleted-since-selection targets submit unchanged (the agent's tool surfaces not-found); chip-only drafts still produce a non-empty prompt.

MVP: the composer trigger/splice contract; the engine-bridge completion seam (section 3 note — no TUI package imports vfs/localfileservice directly); shared noise filter; debounce/cancel/rank/cap; tray sync; exact wire shape; empty-state.

Contracts — consumes: engine-bridge completion seam, session root from session-manager, composer's span/splice API, theme tokens. Emits: outgoing content blocks at submit, tray state ("N files in context"), mention add/remove events. Per the crosscheck it is also the canonical owner of "what does this path/URI resolve to, is it inside an allowed root" for every consumer (diff-view, shell-pane, transcript-view).

Later: cross-turn pinning (and eventually contextasm's unwired KindPinned segments), image mentions (D39), directory mentions, live regrant doorbell, incremental index for huge repos, case-normalization dedup.

Open questions: covered by D39 and the group-14 items (pinning scope, regrant, matcher home).

### 4.13 command-palette

Owns slash-command discovery in the input box: recognizing command typing, a filterable popup with descriptions and argument hints, and routing — remote-recognized lines forwarded verbatim as prompt text (acpsvc's `parseCommand` does the work), TUI-local commands intercepted in-process. The command SET is dynamic and session-scoped; richer locals are contributed by owning components through a registration API this component defines. Per the crosscheck it also owns session modes, the model picker, and the config-option surface (`session/set_config_option`) — rendered/edited via engine-bridge, never re-implemented.

MVP (compressed):
1. Trigger rule mirrors the server exactly (leading `/` on the trimmed buffer) — never stricter than `parseCommand`, so it can never eat a legitimate message.
2. Remote list is whatever the latest `available_commands_update` reported, merged with locals — never hardcoded (`/mission` is capability-gated; external-delegated sessions replace the entire menu).
3. Case-insensitive prefix filter; bare `/` shows the full merged set alphabetically.
4. Up/Down navigate; Tab completes name + one space; Enter submits; Esc closes without touching text.
5. Static argument-hint line (the AvailableCommandInput.Hint verbatim) once a name + space is typed.
6. Unknown tokens pass through unmodified as prompt text — never blocked locally.
7. Dispatch split is explicit and testable: remote names → normal submission; local-only names → in-process handler, nothing sent.
8. Local registry API (name, description, hint, handler); locals shadow same-named remotes; two locals claiming one name is a fail-fast registration panic. Palette itself owns only `/quit` and a client-side `/help`.
9. `/help` answered entirely client-side (D45), rendered as palette chrome, visually distinct from chat.
10. No `/plan` exists; `/mission` is the only fire-async entry point, shown with its server hint verbatim.
11. Empty-input "type / for commands" hint.

Contracts — consumes: available-commands notifications via engine-bridge, local registrations, buffer/cursor read access, (later) value domains for completion. Emits: raw lines to the normal send path, local handler invocations, edit/popup instructions to the composer.

Later: argument-VALUE completion (`/think` levels, `/policy` presets, `/model`, and `/mission`'s agent-name — the last mitigates a documented resolution-ambiguity misfire), mid-session menu re-sync (needs section 3 item 3), `/missions` doorway pending mission-panel's sign-off, fuzzy/frequency ordering.

Open questions beyond D28, D45: final list/naming of locals contributed by other components (session switcher, mission browser, attach) — sign-off needed to avoid collisions (the keymap-registry collision test covers keys, not command names; the fail-fast rule covers the rest).

### 4.14 approval-cards

The inline rendering and keyboard-resolution surface for HITL asks: what is being asked (tool, args, diff, triggering policy/rule, and for mission asks — whose mission), answered with one keystroke without leaving the transcript. Reconciles two structurally different sources: the synchronous blocking ask tied to the session's own turn, and the asynchronous durable cross-process ask raised by a detached mission. No modal takeover, no ceremony, no invented client state.

MVP (compressed):
1. In-session card inline at the point of the call: tool identity, sorted args with visible elision markers (mirroring `hitl_tty.go`'s summariseArg), diff LAST (closest to the decision line) when present, args-only otherwise — never a blank diff section. The diff body is rendered by diff-view (ruling on the round-1 contradiction: approval-cards embeds diff-view; it does not own its own truncation policy — scrollable expand supersedes the CLI's hard 120-line cut).
2. Tool calls execute sequentially per turn, so at most ONE in-session card is ever pending — no in-session queue needed.
3. Exactly two live outcomes: Allow and Deny, one keystroke each, no confirm step; while a card is open the composer doesn't accept chat (modal scope via keymap-registry).
4. Detached-mission queue: pending durable HITLApproval rows with MissionID (from `internal/services/hitlservice` ListPending backfill + `TaskEventApprovalRequested` push), visible/resolvable regardless of the focused session — a persistent badge/count plus an openable newest-first list with MissionID/AgentName/tool/args/policy/rule/countdown per row.
5. Race handling: `ErrApprovalAlreadyResolved/Expired/NotFound` are normal outcomes — remove the row, brief "already resolved" notice, never a TUI error.
6. Countdown from the known deadline in human terms ("auto-denies in 42m"); on_timeout=allow cannot occur (policy validation rejects it).
7. No client-side always-allow memory (D19); every gated call reaches a card.
8. Diff/args copy out as plain terminal text — no widget that eats selection over the body.
9. A denial renders the same framing the engine gives the agent (it may propose an alternative — it isn't stuck).
10. Cancelled turns flip pending cards to a cancelled state rather than leaving them spinning (`forceCancelSessionPermissions` propagated via engine-bridge).

Contracts — consumes: hitlservice via engine-bridge (RequestApproval callback, Respond, ListPending), `TaskEventApprovalRequested`, `internal/store/runtimetypes/hitl_approvals.go` row shape, identity labels from engine-bridge. Emits: allow/deny resolutions, card lifecycle signals (opened/resolved/expired/queue-count) for badges and completion-notification.

Later: an always-allow shape if D19 says so, attention-ask sibling cards (D20), rich diff highlighting, unfocused-arrival notification (delegated to completion-notification), audit-trail pane, bulk queue actions, parity with a future `contenox approvals` CLI (beam is currently the first consumer of ListPending — whether its queue model is the reference shape for that CLI is part of D22's orbit).

Open questions: covered by D6, D19–D21, D23.

### 4.15 diff-view

The shared leaf renderer turning a file mutation — raw `{path, oldText, newText}` or a pre-formatted unified-diff string — into a readable, colorized, copy-friendly block inside tool-call cards and approval cards. Owns diffing (for the raw shape), line classification, truncation/expansion, and exposing plain text for copy. It never decides approval, only display.

MVP (compressed):
1. Both real input shapes: (a) `libacp.ToolCallContent{Type: diff}` — raw before/after with NO server-side markup (pinned by `internal/surfaces/acpsvc/diffcontent_test.go`), diffed client-side; (b) pre-rendered unified strings (mission pending-asks' persisted `row.Diff`), parsed not recomputed.
2. The diff algorithm is the extracted shared engine (section 3 item 8) — no third implementation.
3. Independent size/time bounds inside diff-view: the wire payload is uncapped (full-file rewrites arrive as two complete bodies), so computation and rendering are bounded regardless of the caller.
4. Path header, hunks with line-number gutter, per-line coloring plus `+`/`-`/space prefix glyphs so meaning survives NO_COLOR; new-file (`OldText==""`) and deletion (`NewText==""`) signaled in the header.
5. Collapsed default with explicit expand; full content always reachable by scrolling — never an unrecoverable truncation (deliberately exceeding `hitl_tty.go`'s hard cut, whose "approving accepts changes you have not seen" warning tone carries over to the safety-cap banner with an exact hidden-line count).
6. Copyability: at least one of native selection over the region or an OSC 52 copy keybinding ships (the write itself goes through selection-clipboard — single clipboard owner, resolving the round-1 dual claim).
7. One rendering path serves both call sites (post-execution tool events and pre-execution approval requests).
8. Binary guard: NUL bytes / invalid UTF-8 → "binary file changed, N → M bytes" placeholder (no upstream layer detects this today).
9. Engages only for diff-typed content or a non-empty pre-rendered string; the plain summary/text fallback belongs to the regular content renderer.
10. Memoized computed diff keyed by content hash — resize re-wraps, never re-diffs; degrades gracefully on pathological input.

Contracts — consumes: ToolCallContent diff shape, ApprovalRequest diff fields via approvalflow, missiontools PendingAsk detail, color-capability from theme-styles, clipboard writes via selection-clipboard. Emits: the rendered block for embedding cards, raw-text copy payloads, expand/scroll state for card layout.

Later: side-by-side mode, per-language token highlighting (a lexer is a real new dependency), word-level intra-line diffs, per-hunk folds, multi-file aggregation, configurable context lines.

Open questions: extraction-vs-dependency for the algorithm (section 3 item 8's "how"); scrollable safety-cap threshold; whether binary/size guards should also move upstream so every consumer benefits; whether the two input shapes should be unified at the service layer or permanently both-supported.

### 4.16 mission-panel

beam's window onto the fleet: fire a bounded unattended unit at a declared agent, watch it, read what it reports without leaving the flow, stop it when needed. Dispatch, envelopes, and routing already exist in-process — the panel is a thin honest rendering plus two write actions (fire, stop). A mission is fire-and-forget by design: "detach" is not an action, it's what firing already did; the panel's job is making what comes back legible and never silently lost.

MVP (compressed):
1. Two fire entry points resolving to ONE call: a structured form (agent picker from the registry, one-line intent, envelope picker, prefilled from `default-mission-agent`/`default-mission-policy` KV) and typed `/mission [agent] <intent>`.
2. Capability gating identical to `hasMissionCapability` (configured default model, not itself a dispatched unit); unavailable → visibly disabled with the same explanatory text acpsvc gives ("configure one with `contenox config set default-model`"), never a silent no-op.
3. Every dispatch requires agent AND envelope; missing either → the same readable rejection `handleMission` gives.
4. Successful fire shows the new mission immediately (agent, envelope, intent) — no guessing whether it worked.
5. Listing scoped per D24, newest-first: agent, envelope, truncated intent, status (open/landed/derailed/stuck/abandoned) plus a liveness qualifier when open (from LastHeartbeat/LastError, rendered via the shared liveness primitive) plus a distinct "waiting on you" sub-state when attention asks are pending.
6. Durable across restarts: missions fired in a previous run still list.
7. Reports arrive on the ordinary session stream tagged `_meta["contenox.missionReport"]`; the panel updates the row (last-report summary, unread indicator, kind tally) off the same stream — no separate poll; transcript rendering of the card is transcript-view's.
8. Detail view backed by `missionservice.Get` + `ListReports` — the durable record independent of transcript scroll; report rows show kind/timestamp/summary with collapsed expandable detail/refs/handover.
9. Plan progress rendered from the Mission record read in-process (entry counts by status + latest explanation); absent when `Plan.Revision==0` — nothing, not an empty bar.
10. Stop: one confirming keypress; calls `fleetservice.Cancel`/`Stop` AND `missionservice.Finish(StatusAbandoned, "stopped by operator")` — verified: Stop alone leaves a phantom-open mission (D25).
11. Dispatch/Stop/Cancel errors surface as readable inline messages.
12. Empty states matter: no missions → inviting copy naming the fire key; capability unavailable → the explanatory text.

Contracts — consumes: fleetservice (Dispatch/Stop/Cancel/List/Get), missionservice (Get/List/ListReports/Finish), agent registry, policy names, KV defaults, the capability signal and report-tagged stream from engine-bridge, mission-supervision helpers (`MissionsFiredBy`/`PendingAsks`). Emits: dispatch requests with ParentSessionID, stop/cancel + Finish, report-recognition signals to the transcript, mission status/report events consumed by completion-notification, the running-count badge (single named publisher for app-shell, resolving the round-1 dual-sourcing).

Later: cross-process fleet board, operator-inbox integration (D22), sub-mission roll-ups, plan revision timeline, attention-answer UX handoff (D20), filtering, re-fire-from-Handover.

Open questions: covered by D24–D30; keybindings defer to keymap-registry.

### 4.17 shell-pane

beam's surface for the user's own ad-hoc shell lines: a prefixed composer line runs on the chat session's persistent PTY-backed shell instead of starting an LLM turn, output rendered live in a toggleable, append-only, color-aware, easy-to-copy pane. A thin layer over `internal/services/shellsession`, which already encodes the hard decisions: one PTY per session, line-gated submission, reference-only context, no interactive-PTY fidelity.

MVP (compressed):
1. Receives composer-classified prefixed lines (composer owns interception — ruling above); a bare prefix falls through as chat.
2. One line → one `shellsession.Manager.Run` via engine-bridge; no multi-line submission.
3. Persistent session-scoped shell created on demand, rooted at the workspace root via the vfs allowlist; cwd/env/history/jobs persist across submissions and turns; idle-reaped (~15 min) and transparently recreated.
4. Non-blocking live streaming (Run returns after the ~250ms capture window; output streams in on the ~60ms fan-out); no artificial per-command timeout (vibe's 30s kill is gone).
5. Interrupt without teardown: a dedicated key writes `\x03` into the PTY; killing/resetting the shell is separate and explicit.
6. Bounded scrollback (64KB ring) with a visible truncation banner once bytes are evicted.
7. ANSI: SGR parsed and rendered via theme-styles; everything else (cursor motion, OSC, alt-screen, bracketed-paste toggles) stripped — a defect already found and fixed once in the deleted web frontend's sanitizer; parser state carries partial escapes across chunk boundaries.
8. Full-screen/cursor-addressed programs are a documented limitation (help text), not silent breakage.
9. Reference-only context, hard: shell output NEVER auto-injects into model context (deliberate reversal of vibe's synthetic-user-message behavior); the agent has its own ungated `shell_session_read`. An optional compact `$ <command>` transcript marker is a log affordance only.
10. Toggleable pane: auto-opens once per session on first output; an explicit close is respected thereafter.
11. Session switch re-subscribes and repaints from the Reset snapshot (replace, not append); other sessions' shells stay running.
12. Copy stays trivial: the renderer only appends — never redraws emitted lines — so native selection works (hard constraint, not a side effect).
13. Nil Manager → explicit "shell sessions are disabled" message.
14. The user's own line is never HITL-gated (existing "user's own machine and keyboard" rationale); agent-initiated shell calls remain a separate, already-built approval surface.

Contracts — consumes: shellsession Manager (Run/Read/Subscribe/Kill) via engine-bridge, session lifecycle events, liveness for the running-command header. Emits: rendered pane output (never context), optional transcript markers, interrupt writes. Per the crosscheck it also hosts agent-spawned `terminal/*` tool-call terminals (ToolCallContent kind=terminal carries only a TerminalID).

Later: raw keystroke co-input ("Phase 3" in the existing design record), true PTY fidelity (possibly on `internal/services/terminalservice`, the orphaned full-duplex attach service — D10's orbit), multiple shells per session, explicit promote-to-context (D11), pane-local history, PTY resize, interleaving warnings for concurrent user+agent commands.

Open questions: covered by D5, D8–D11; plus re-instating the deleted `shell-sessions.md` decision record (recommend porting its rationale into docs rather than losing it).

### 4.18 selection-clipboard

Copy-out and paste-in as a first-class subsystem: text out to the system clipboard reliably (local or SSH/tmux), text and eventually images in without corrupting input. It's a named component because the two "free" copy mechanisms — native mouse selection and OSC 52 — conflict with mouse-driven UI, alt-screen scrollback, and each other's multiplexer prerequisites; the trade is made deliberately here.

MVP (compressed):
1. No terminal mouse-capture mode by default (D2 ratifies) — click-drag stays the terminal's native selection with zero configuration.
2. Explicit mouse-independent copy keybindings: last assistant answer (excluding thinking/chrome), last user message, Nth fenced code block of the latest answer (visible index badges), last shell-output block — required because alt-screen has no OS scrollback (interacts with D1).
3. Copy via OSC 52: pure-Go, no external binaries, identical over SSH. Beam is the single clipboard writer — diff-view and everyone else route through it.
4. tmux/screen DCS passthrough wrapping when `$TMUX`/`$STY` is set; docs state the one-time tmux config beam can't set itself.
5. OSC 52 is fire-and-forget: every copy shows an immediate "sent" confirmation (never claimed "worked") plus a one-time first-use hint.
6. Payload size capped; truncated copies say so explicitly.
7. Bracketed paste: every block is literal insertion — newlines preserved, no auto-submit, no slash/shell trigger interpretation; only an explicit post-paste Enter can trigger anything.
8. Large pastes collapse in the composer ("[pasted 42 lines / 1.8KB]") with expand/preview.
9. Image paste MVP is file-path based: a pasted/typed path resolving to a local image → prompt to attach → raw bytes + MIME handed onward (policy ownership D37).
10. Every keybind is in the discoverable help (via keymap-registry); names never collide with server slash commands.
11. Per the crosscheck it owns invoking `FlattenContent` for copy-out and surfacing the "N items not sent/copied" dropped-kind notice.

Contracts — consumes: transcript/navigation focus state (read-only — `CurrentFocusItemID`, LineRange) to locate copy targets, composer draft/cursor for insertion, env signals ($TERM/$TMUX/$STY, tty presence following the existing `hitl_tty` platform-split convention). Emits: escape sequences to the terminal, confirmation notices, candidate image attachments, its keybind table into help.

Later: vim-like range select mode, true binary image paste per-terminal (D36), opt-in mouse affordances, a `/doctor`-style clipboard capability probe (recommended early enough to validate the terminal matrix empirically), OS-tool fallback, paste-burst heuristics.

Open questions: covered by D2, D34–D37; the per-terminal OSC 52 support matrix needs empirical testing, not assumptions.

### 4.19 context-budget

The token/context budget surface: display usage vs the effective limit, warn before the window fills, and guarantee a way out of an overflow. Directly answers the maintainer's motivating failure: **a session wedged at 19,734/19,727 tokens** because the oversized content was already committed to history and no non-LLM recovery path existed.

Acceptance criteria (keep):
- A session in that exact failure shape must be recoverable through an in-TUI action that (a) requires no LLM call and (b) cannot itself fail from being over budget — a mechanical trim, not just `/compact`.
- The gauge is always visible while attached and updates within one turn of any `usage_update`.
- Crossing 75%/90% changes the gauge state automatically.
- A `max_tokens` stop (or the error family, pre-fix) produces a persistent banner — not a toast — naming both recovery actions and which one is guaranteed.
- beam performs zero token counting, shifting, or summarization; every number and action is service-sourced.

MVP: status-bar gauge `{used}/{size} ({pct}%)` from `usage_update` only (already emitted at session start, pre-turn, and post-shift — no new plumbing for display); three visual states via theme-styles roles; overflow banner offering `/compact` ("uses the model — may fail if already over budget") and the new mechanical trim ("always works, loses that content") — both routed as ordinary slash commands; advisory don't-resend guidance only (the server's poison-pill guard enforces; no client-side blocking); never lock the composer on overflow — the user must be able to type `/trim`.

Blocked on section 3 items 2 (stop-reason fix — without it beam can't distinguish overflow from generic errors without forbidden string-matching), 10 (TrimHistory + command), 11 (per-step poison-pill), 13 (shift event, later-tier).

Contracts — consumes: usage updates and StopReason via engine-bridge, command entries via the palette. Emits: slash-command invocations only.

Later: configurable thresholds, cost display once `UsageCost` is populated, selective per-message drop (needs per-message sizes chatservice doesn't expose), CacheClass breakdown (gated on the T3 context planner), shift-transparency notes, per-mission budget (D30).

Open questions: covered by D30–D33.

### 4.20 completion-notification

Releases the user's attention back when something finishes or needs them, instead of holding it via constant motion: terminal bell / OSC 9 when a turn ends, a mission reaches terminal status or files a consequential report, or an ask is raised — but only when the user isn't already looking at it. A pure downstream consumer; no new bus subject, no service behavior.

Acceptance criteria (compressed): turn end rings once for StopReason in {end_turn, max_tokens, max_turn_requests, refusal}; cancelled never rings (the user drove it); mission terminal statuses {landed, derailed, stuck, abandoned} ring, StatusOpen never; report kinds blocker/result ring, progress/finding never; approval/attention asks ring regardless of origin (bypass question D23); suppression = window focused (bubbletea FocusMsg) AND the event's owning surface is active; burst coalescing (default 1.5s) → 1 ring; rate floor (default 2s); idempotent by event identity across reconnect/replay; refocus resets throttling but never replays; global off switch means zero escape bytes ever; OSC 9 bodies carry only a generic label ≤80 chars — never tool args/diffs/report detail (banners can surface on a locked screen outside the terminal's trust boundary); all emission routes through app-shell's owned output path (never a bare background-goroutine stdout write that corrupts a frame); BEL is the only always-safe signal — OSC 9 only on an env-heuristic allow-list (iTerm2/WezTerm/ghostty/Konsole), skipped entirely under $TMUX (BEL only, relying on tmux's own bell flagging).

MVP: the four trigger rules; focus tracking via `tea.WithReportFocus` (app-shell must enable and forward); active-surface comparison via session-manager's mapping (the primary fire-and-detach scenario: same window, different tab); throttles; idempotency; toggle (storage: D46); best-effort OSC 9 with redaction; tmux special case. Default ON with an easy off-switch — the premise is calling the user back.

Contracts — consumes: StopReason per session from engine-bridge, mission status/report events as routed by mission-panel, ask-raised occurrences from approval-cards, focus state from app-shell, surface mapping from session-manager. Emits: raw escape sequences only — a terminal leaf, not a producer. Service extensions: none.

Later: OSC 99 (kitty), OSC 777, escalating reminder near an ask's ExpiresAt, per-kind configurability, native OS notifier opt-in, quiet hours + per-session cap, empirical terminal matrix verification.

Open questions: covered by D23, D46, D53.

### 4.21 test-harness

The shared testing infrastructure that keeps beam agent-verifiable: a teatest golden/snapshot harness, a canonical fixture corpus of engine events every component's tests replay, and the frame-diff instrumentation that regression-tests the liveness metric. Without it, beam is the one surface agents cannot self-check via `task test-unit`.

Acceptance criteria (compressed): all beam golden + liveness tests run headless in the existing `TestUnit_*`/`-short` CI gate — no PTY, no LLM/network, zero flakes over 20 reruns, low-seconds runtime; a versioned fixture corpus derived from the real shapes in `internal/surfaces/acpsvc/events.go` and `internal/kernel/taskengine/events.go` (streaming chunks, thoughts, tool lifecycle, token_usage, plan, approval, chain terminal states) reused by every suite; ≥1 golden per component via teatest, byte-exact, regenerated by one documented `-update` flow; the LIVENESS regression pair — active fixture (streaming chunk, or a mission heartbeat with no output) must produce non-byte-identical consecutive frames across ≥3 simulated ticks, and idle fixtures must stabilize (catches both the frozen frame and the spinner that never stops); pinned terminal sizes (80x24, 120x40) via WindowSizeMsg, never the runner's real env; one shared package (fixture loader, golden helper, frame-diff helper, FakeEngineBridge); failure output reads as an actionable diff so an agent can self-correct from `go test` output alone.

MVP: introduce the charmbracelet deps (with theme-styles, this is the TODO §8 arrival); fixture corpus as source of truth; FakeEngineBridge implementing engine-bridge's event contract (blocking dependency: that contract must be fixed first), deterministic, injected clock; documented golden convention (path layout; ANSI-vs-plain is D54); published "frame"/"tick" vocabulary; the two maintainer-named liveness cases (mission fire-and-detach heartbeat; transcript streaming); Taskfile/ci.yml wiring preserving the no-Docker/no-LLM guarantee; the contract doc: every render-touching PR adds/updates a golden or liveness test.

Contracts — consumes: engine-bridge's event contract, real wire types, each component's tea.Model. Emits: the double, the corpus, the helpers, the size constants, the CI task. Service extensions: none (possible injectable clock/ids, section 3 item 4).

Later: shell-pane/diff-view/theme goldens (paired light/dark), fuzz of truncation/wrapping, vhs-style recordings (human review, out of scope), CI job split if slow.

Open questions: covered by D54–D56; plus an audit that no rendering path reads real OS terminal state once app-shell/composer exist.

## 5. Cross-component contracts

The seams multiple components share; each has exactly one owner.

- **The engine-bridge event stream is the single source.** Every fact rendered anywhere — text deltas, tool status, usage, plan, mission reports, approvals, commands menu — arrives as one typed event on engine-bridge's per-session channel, in wire order. liveness derives ActivityState from it; transcript-view renders from it; context-budget reads usage from it; completion-notification classifies from it; FakeEngineBridge replays it in tests. No component subscribes to a service bus or imports acpsvc/libacp wire types directly.
- **liveness ActivityState feeds transcript, mission-panel, shell-pane, approval countdown ticks, and app-shell's status line.** Consumers pull spinner-frame and elapsed strings at render time; nobody tracks its own clock or invents a waiting flag. The 50%-micro-motion and 1s-freeze criteria are properties of this one primitive, verified once by test-harness.
- **keymap-registry is the sole key arbiter.** Raw KeyMsg enters only there; components receive semantic Actions in their declared scope; modals trap via focus push/pop; Escape walks one priority stack; the help overlay and status-bar hints are generated from registrations. app-shell composits; the registry decides. The collision test in CI is the enforcement mechanism.
- **Selection vs navigation seam.** transcript-navigation owns the focus cursor, search, and fold booleans, exposed read-only (`CurrentFocusItemID`, positions); transcript-view owns rendering and exposes `Items()`/`LineRange()`/`ScrollToItem()`; selection-clipboard consumes focus state to seed copy targets and is the only OSC 52 writer (diff-view and others hand it payloads). Navigation never calls the clipboard; clipboard never moves the viewport.
- **Composer classifies; others execute.** The composer tags a submission as text/command/shell (+ mentions, + staged images); command-palette intercepts local-only commands; shell-pane receives classified shell lines; file-addressing interprets `@` spans through composer's splice API; engine-bridge submits everything else.
- **first-run's Ready gate and connection-lifecycle's status vocabulary are the only readiness truths.** No component imports setupcheck or invents health states; the status bar renders connection-lifecycle's closed vocabulary; model-requiring actions gate on first-run's Ready value.
- **Notification suppression needs two inputs owned elsewhere**: window focus (app-shell enables `WithReportFocus` and forwards) and the active-surface mapping (session-manager). D53 must define "active surface" broadly enough to cover non-session surfaces.
- **Mission badge has one publisher**: mission-panel (scope per D24). app-shell renders it; nothing else counts missions.
- **The shared toast/notice primitive (D48) must exist before five components ship five timers.** Whoever owns it, the convention is fixed: single line, inline, auto-dismiss, never modal.

## 6. ACP crosscheck results

The crosscheck audited every exported libacp type/const against the 21 components. The **unowned** residue — the plan's residual holes, verbatim:

1. "MCP-server-active status readout: no component shows 'which MCP servers/tools are wired for this session' or explains a tool-call failure caused by an unreachable MCP server; recommend extending acpsvc's /doctor (handleDoctor, runtime/acpsvc/commands.go) to include MCP allowlist status and letting command-palette surface it, rather than inventing a new surface."
2. "Extension ('_'-prefixed) method/notification rendering: engine-bridge plumbs them through, but no component owns presenting an unrecognized extension's effect to the user (e.g. a future agent-specific capability). Likely intentionally out of MVP scope, but flag explicitly so it isn't assumed covered by transcript-view."
3. "AuthEnvVar secret/masked-field collection UI details (Secret pointer defaulting true, Optional vars) — first-run owns the wizard overall, but no component spec pins down the masked-input widget itself; this is a composer-adjacent text-input concern that should be named explicitly (extend composer's input widget for masked/secret mode, or first-run builds its own) rather than assumed."

Notable owned-mappings worth recording because they were surprising:

- **libacp's Authenticate machinery is not a login system.** `internal/surfaces/acpsvc/authenticate.go` implements it as a one-time terminal/browser/env setup wizard, and Logout is permanently MethodNotFound. Auth therefore belongs to first-run, not connection-lifecycle, and nobody should build a re-auth/session-expiry UI beyond routing back to setup.
- **liveness vs connection-lifecycle boundary is deliberate**: connection-lifecycle owns "is the engine/service reachable" (handshake, drain-on-shutdown, EngineDown); liveness owns "is THIS turn/mission still making progress" (stall, heartbeat). A maintainer must be able to point at exactly one owner for "nothing has happened in 30s".
- **Plan updates default to mission-panel** as the signature artifact, but a plain session can also emit them — the fallback question is D29, carried forward rather than silently dropped.
- With first-run understood as owning setup/auth, the 12 + 9 components cover the libacp inventory completely; both remaining unowned items are small (a status readout, a widget detail) — no capability-sized gap.

## 7. Build order

The round-1 critic's ranked order, merged with the gap components. Foundational infrastructure (test-harness, keymap-registry, liveness) moves early per the maintainer's instruction; one line of reasoning per slot.

1. **test-harness** — introduces the TUI deps and the golden/liveness metric before the first pixel exists, so every later component is born verifiable (its FakeEngineBridge is finalized as soon as slot 3's contract is).
2. **theme-styles** — zero dependencies; every View() call goes through its roles, so it must exist before any screen is drawn.
3. **engine-bridge** — the seam to acpsvc/enginesvc/hitl/fleet; nothing shows real data or submits a prompt without it, and its event contract unblocks the FakeEngineBridge.
4. **keymap-registry** — the sole key arbiter must exist before any component writes an Update() that handles a key, or the collision guarantees are retrofits.
5. **liveness** — ActivityState over engine-bridge's stream; building it before any activity-bearing view means transcript/mission/shell/status render from it instead of ad hoc waiting flags (the exact regression being guarded against).
6. **connection-lifecycle** — wraps engine-bridge call boundaries with the state machine and recover() from day one; its vocabulary is what the shell renders.
7. **app-shell** — program root, geometry, focus hosting, the D1 decision locked before children's Update/View are written against it.
8. **first-run** — the actual first frame; needs app-shell's mount and engine-bridge's SetupCheck; everything after it can assume Ready.
9. **session-manager** — resolves/loads the active session so there is something for the transcript to render.
10. **transcript-view** — the core read loop; the "true coding TUI" feel lives here and must be real before composing feels real.
11. **composer** — input/submit/classification; depends on the focus contract and SubmitPrompt.
12. **diff-view** — leaf renderer built once, before approval-cards and tool cards both need it (and after the shared diff engine extraction).
13. **approval-cards** — depends on engine-bridge's AskApproval plumbing, diff-view, and the transcript card slot.
14. **command-palette** — depends on composer trigger detection and the AvailableCommands stream.
15. **context-budget** — pure rendering over the usage stream plus two commands; slotted after the palette so `/trim` is dispatchable, and after its section-4 service work lands.
16. **transcript-navigation** — an index over transcript-view's stable Items() API; pointless earlier.
17. **file-addressing** — needs composer's span/splice contract and engine-bridge's completion seam.
18. **selection-clipboard** — copy targets ("last answer", "block 2", focused item) only become real once transcript, composer, diff-view, and navigation exist.
19. **shell-pane** — depends on composer classification and the shellsession seam; off the critical path of the core chat loop.
20. **mission-panel** — the signature feature but the highest-dependency component (fleet wiring, report cards, ask queue, liveness rows); built last-but-one so it composes stable pieces instead of driving their design.
21. **completion-notification** — a pure leaf consumer of turn/mission/ask events plus focus state; wiring it last means every event source it suppresses against already exists.

## 8. Prior art

The old vibe TUI (recoverable from git history; a working copy was staged during this pass) is the direct ancestor and the calibration for both what to keep and what to never repeat. It got the substrate right: a warm in-process engine with persistent MCP connections across turns, and the `$`-shell passthrough as a genuinely fast mechanic. Its plumbing lessons carry over too — rune-safe truncation after a multibyte panic, the y/n approval key-interception pattern, minimum-size layout math. But its measured failure defines beam's headline acceptance bar: a static `waiting` bool that swapped one glyph at turn start and end — no spinner, no tick, no elapsed clock — produced **82–87% frozen frames during active turns, including one 48.6s byte-identical still frame**; its `tea.WithAltScreen()+tea.WithMouseCellMotion()` construction broke native text selection, directly contradicting the copy/paste requirement; and it auto-injected shell stdout into model context as a synthetic user message, a behavior the shellsession design has since deliberately reversed. The maintainer rejected its control-panel-shaped sidebar-of-declared-state design, not its mechanics: beam keeps the warm engine and the shell reflex, and replaces everything about how the frame earns the user's trust.
