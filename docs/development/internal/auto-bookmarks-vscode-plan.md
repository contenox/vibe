# Wellworn: automatic bookmarks, rebuilt AI-native for VS Code

## 1. Recommendation

Build it, small. The paper's headline result is a null: automatic bookmarks did
not reduce navigation count (p.31, explicit). What survives is narrower —
comprehension cost is real, manual bookmarking has real friction, and the
paper's own pivot (fewer distinct files touched, not fewer navigations) points
at the right metric. That metric maps directly onto LLM context: a small,
ranked "where has this session's attention actually been" signal, fed as an
ACP `resource_link` alongside whatever the user explicitly attaches. Ship it
as gutter decorations plus one quick-pick command first (client-only, no
kernel changes, cheap to cut). Gate the model-feed half behind Phase 3 of the
existing VS Code plan (attachment chips) rather than inventing a channel. Do
not build a bookmarks panel, do not chase the original navigation-count
metric, do not claim the LLM will benefit before dogfooding says so.

Name: **Wellworn** — settled by the maintainer 2026-07-28 ("any of yours is
good enough"), so the naming section below is history, not an open question.

**Scope, as narrowed by the maintainer after this doc was drafted:** an
internal algorithm serving the assistant, not a navigation feature. The two
developer-facing surfaces are a right-click action on a selection (explain
this, grounded in where I have been working) and an on-demand "hottest
sections, each with an LLM one-liner" view. See §4.

## 1a. Containment — Wellworn must not leak

**Wellworn is a VS Code-only feature. The runtime must never learn it
exists.** This is an invariant, not a preference: the whole reason it is
affordable is that it costs the shared core nothing.

Everything lives in `packages/vscode/`. Its output reaches the model as
**ordinary prompt context** — a `resource_link` or plain text attachment,
byte-indistinguishable from the developer attaching a file by hand. The
runtime sees context, not a concept.

Hard prohibitions, no exceptions:

- No changes under `internal/kernel/` or `internal/services/`.
- No new tool and no tool-registry entry.
- No schema, store, or `runtimetypes` change; attention data is
  session-scoped VS Code state and dies with the window unless the extension
  persists it in its own storage.
- No engine vocabulary. Nothing in the runtime is ever named "Wellworn"; it
  runs a chain it was handed and sees context, not a concept.

**Settled by the maintainer 2026-07-28: the explain half is a chain WITH
TOOLS**, not a one-shot prompt. The agent must be able to explore files and
use the harness to find similar and dependent files. That is not context
assembly; it is an agentic loop.

The tools already exist for ACP sessions (`internal/surfaces/contenoxcli/acp_toolset.go`):
`gointel` gives dependents and callers (`go_references`, `go_implementations`,
`go_symbols`), `workspace_search` gives semantically similar code with
`file:line-range` citations, plus `git`, `jq`, `goja`. A Wellworn chain needs
no new tool — that hard prohibition stands and is not strained by this.

**The open question is now narrower: how does the client select that chain?**
Today it cannot. `ChainRegistry` holds exactly one `defaultChain`
(`acpsvc/chain.go:23-26`) and both prompt paths hardcode `.Default()`
(`prompt.go:237`, `native_turn.go:319`). One session, one chain. Two ways out:

- **(A) A Wellworn extension method** — `_contenox/wellworn/explain` with its
  own chain registry and toolset, mirroring `_contenox/autocomplete` +
  `chain-fim.json`. Known cost, precedented, ships fastest. But it puts a
  Wellworn-shaped method on a protocol surface every ACP client sees
  advertised, including clients that can never use it.
- **(B) Per-session chain selection as a generic ACP capability** — make the
  registry name-keyed and let a client choose the chain at `session/new`.
  Then Wellworn is *literally just a chain file*, which passes the
  concept-vs-file test outright, and the capability is one any ACP client can
  use (a Zed user could select a review chain). More work in `acpsvc`, which
  is a surface, so the standing constraint permits it.

**Recommended: (B).** It converts a Wellworn-specific leak into a general
capability, and it is the one that keeps the containment claim honest rather
than merely technically satisfied. Take (A) only if (B) proves expensive once
scoped.

Either way the explain loop should run in its **own** ACP session, so the
chat transcript is never polluted; and its tools should be read-only, so it
never triggers an approval storm.

## 2. What the paper actually found

A. Ertli, *On the impact of automatic bookmarks for prediting [sic] navigation
cost through sourcecode*, Bachelor's thesis, Universität Bremen, informatics,
26 Dec 2021, supervised by Martin Schröer, first reviewer Prof. Dr. Rainer
Koschke, second reviewer Abir Bouraffa (Universität Hamburg). 45 pages. Read
in full via the archived PDF.

**Problem (p.3).** Program comprehension dominates developer time: Fjeldstad
& Hamlen (1979) put maintenance-phase comprehension at 50%; Xia et al. (2018,
IEEE TSE) put it at 58%. Developers spend 35–50% of total work time
navigating code. Piorkowski et al. (FSE 2016) found developers cannot
reliably judge in advance whether a given navigation is worth its cost — they
routinely misjudge it. Ko et al. (2006, IEEE TSE) found developers foraging
for relevant fragments repeatedly return to the origin of a navigation path.
Guzzi et al. (ICPC 2011) surveyed developers and found bookmarks are rarely
used despite being reported useful — a "blind spot," the thesis argues, by
analogy to Oliveira et al.'s (ACSAC 2014) finding that security is a similar
blind spot in the dev process. Moritz Weinig's prior Master's thesis (2020)
built the original "Autobookmarks" concept to remove the overhead of manually
setting bookmarks, and found acceptance high — but never rigorously tested
whether autobookmarks change navigation behavior. That gap is this thesis.

**Research question (p.4):** can automatic bookmark-setting reduce the number
of navigations a developer needs to complete a task? "Navigation" is defined
precisely as bringing an Eclipse editor file to the foreground (p.16).

**Mechanism (§2, pp.4–11).** An Eclipse plugin built on Mimesis, a DFG-funded
framework that records every IDE interaction during unknown-code
program-comprehension studies (Schröer & Koschke, SANER 2021). For each
source line, a "Degree of Interest" score (styled on Kersten & Murphy's Mylar,
2005) accumulates from a weighted sum of interaction-event types (Table 1,
p.8):

| Event | Weight |
|---|---|
| textSelectionEvent | 1 |
| viewEvent | 0.5 |
| editorTextCursorEvent | 0.9 |
| editorMouseEvent | 0.8 |
| scrollEvent | 0.1 |
| codeChangeEvent | 0.9 |

Weights were hand-tuned iteratively, not derived formally, specifically to
offset how much more often some event types fire than others (codeChangeEvent
fires per character; textSelectionEvent, once per interaction) — three
developers pilot-tested the final weighting (p.9). A decay constant
(0.0001 per update, p.10) fades scores that stop accumulating, so idle files
don't all end up bookmarked. Score bands gate visibility: >40 "very
relevant," >20 "relevant," >1 "slightly relevant," >0 "neutral" — neutral
bookmarks are hidden from the user (p.7). A masking step (inherited from
Weinig) prevents clustering: bookmarks are placed on the top-scored line,
then the next ~30 line-entries are skipped before placing the next one
(pp.10–11). The bookmark itself is a gutter/line highlight plus an entry in a
ranked, double-click-to-jump list view (Fig. 1, p.5). The author states two
intended uses (p.6): jump back to a flagged fragment instead of re-walking a
dependency chain, and recognize a previously-explored dead end fast enough to
abandon it.

**Method (§3, pp.11–19).** A remote, largely unsupervised study: recruits
(university CS students/staff) ran a demographic survey, then a program task,
then an end survey, all through a browser-triggered RDP-gatewayed VM with
Eclipse pre-configured — no local setup. The task, reused from the Mimesis
DFG study for comparability, was a real JabRef bug fix: a faulty object
instantiation passing `null` instead of an already-initialized
`clipBoardManager` (Listing 4, p.14). A second, purpose-built XML log
recorded every bookmark-repository event (create/show/hide/update/delete,
with score, file, line, timestamp — Table 2, pp.17–18), which the author
needed to reconstruct exactly which bookmarks were visible to a participant
at any moment, since no experimenter was present. Weinig's own prior dataset
was lost to a software bug (p.16), so this thesis is the first analyzed
dataset for the concept at all.

**Results.** Recruitment funnel (Table 3, p.19): 35 completed the
demographic survey → 29 started the desktop session → 19 read the task → of
those, only 7 solved it (Group C3), 12 tried and failed (Group C2), 16 had
already dropped out on reading the task. The author attributes the low
completion rate to deliberate task difficulty and Eclipse's unpopularity
among the recruit pool (§4.1, p.29), not to the tool. **Primary result
(§4.2, p.30f):** Group C3 (autobookmarks, solved, n=7) needed a mean 30
navigations (σ=22); Group D, the pre-existing Mimesis baseline without
autobookmarks (solved, n=13), needed a mean 30 navigations (σ=13) — identical
means, higher variance with the tool. A combined scatterplot (Fig. 15, p.31)
shows no cluster separation between groups. Author's own words, bolded in the
original (p.31): *"Die ursprüngliche Annahme, dass die Navigationsanzahl eine
geeignete Metrik ist, um die Navigations-Performance zu messen ist daher so
nicht haltbar"* — the original assumption that navigation count is a suitable
metric for navigation performance does not hold — and, just before it: one
can nonetheless rule out that the plugin reduces overall navigation count.
This is a stated null on the pre-registered primary metric, not an
inconclusive shrug.

**End survey (§4.3, p.32, Table 6).** Among the 7 who solved the task,
self-reported bookmark usage was low — most said "no" or "little" to having
used them; only 2 of 7 thought bookmarks even *maybe* helped avoid
unnecessary navigation. The author notes this is nonetheless better than
Weinig's own end-survey figures (Tables 7–8, p.32), where almost no one found
the bookmarks helpful or used them at all.

**Secondary finding (§5.2, pp.35–38), explicitly hedged.** Navigation counts
matched, but distinct files opened did not: baseline Group D opened a mean 13
unique files (σ=6) to reach a solution; autobookmarks Group C3 opened a mean
10 (σ=3). The author reads this as suggestive that autobookmark users
abandoned unproductive paths faster / stayed inside a smaller working set,
but states directly: *"ohne weitere Datensamples ist es nicht möglich, diese
Beobachtung zu erklären"* — without more data samples this observation
cannot be explained (p.38). Individual navigation-graph case studies (pp.20–28)
show the dominant failure mode was not "wasted navigation" but "never
recognized the bug once looking at the right file" (e.g. user-E1ML: found the
buggy file early, worked it intensively, never diagnosed it) or "found no
foothold at all" (user-GFDL: "I'm pretty rusty with Eclipse and Java. No idea
where to start.").

**Threads of validity, author's own list (§6, pp.38–39):** C3 and D are
different participant pools, not a randomized comparison; too few samples (7
vs. 13) for a firm conclusion; technical differences between the two study
setups could confound results; the Autobookmarks invitation text may have
revealed slightly more about the task than Mimesis's did; the JabRef task may
not have been suited to studying navigation behavior at all; two sessions hit
technical problems requiring an Eclipse restart, risking recording data loss;
and — the one worth carrying forward — Autobookmarks participants were told
upfront (via the study landing page, Fig. 2, p.13) that the plugin "should
help navigate faster between often-visited code spots," priming them to
expect their navigation would be judged, unlike Mimesis participants who were
never told their navigation would be analyzed. The author names this demand-
characteristics confound directly as a possible full explanation for the
files-visited difference.

**Conclusion (§7, pp.39–40).** With navigation count as the metric, no effect
on navigation count could be shown. Among developers who reached a solution,
autobookmarks were rarely used, and the concept in this form did not convince
most participants of its usefulness. The thesis argues navigation count alone
is insufficient to measure comprehension effort and proposes files-touched
relative to navigation count as the better lens, calling the observed
narrowing suggestive, not proven, and asking for a larger follow-up. It
separately claims the remote-study method itself (RDP-gateway, no local
setup, resumable sessions, automated navigation-graph visualization) worked
well and is reusable infrastructure for future studies. Future work proposed:
analyze the unsuccessful participants' recordings too, to find where they got
stuck.

## 3. What survives into an LLM world, what does not

**Survives, and is load-bearing:**

- The underlying comprehension-cost problem (Fjeldstad/Hamlen, Xia, Ko,
  Piorkowski) is about human cognition and code structure, not 2021 tooling.
  It is, if anything, worse in 2026 with larger frameworks and codebases —
  the thesis's own framing (p.3).
- Manual bookmarking's blind spot (Guzzi et al.: useful but unused because
  maintaining it is itself work nobody prioritizes) is a friction problem
  independent of era. Automating the *setting* of bookmarks was already the
  right instinct; it just wasn't validated to change behavior.
- The DOI engineering pattern — weighted interaction events, decay,
  minimum-distance masking to avoid clustering — is a sound, reusable
  algorithm shape regardless of what consumes the output. It doesn't need an
  LLM to make sense.
- Low observed usage in the end survey is a signal to take seriously in any
  era: if the affordance costs attention to check, people won't check it. A
  2026 rebuild has to be ambient (decorations, not a panel to open) or it
  will repeat this exact failure.
- The thesis's own reframe — file-count/working-set narrowing over raw
  navigation count — maps directly onto the new goal. A small, high-signal
  working set is *exactly* what you want to hand a model as context: fewer,
  more relevant files means cheaper prompts and less irrelevant grounding.
  This reframe is the single most important thing to carry forward, but note
  it below for what it actually is: suggestive, n=7 vs n=13, and possibly a
  demand-characteristics artifact (thread of validity #7, p.38) rather than a
  real tool effect. Weak evidence, not proof.

**Does not survive, or needs replacement:**

- "Navigation count" as the target metric is explicitly falsified by the
  paper's own primary result (p.31) — it should not become this feature's
  success criterion either.
- The Mimesis instrumentation layer doesn't need reconstructing. VS Code
  ships first-class events (§4) that a DOI tracker can subscribe to directly
  — no bespoke recorder required, which is the whole reason the maintainer
  wants this in VS Code rather than the TUI.
- The always-open, double-click list-view UI (Fig. 1, p.5) is the wrong
  shape for a solo-maintained extension: a new heavyweight panel that must be
  opened to be useful is the same friction pattern Guzzi et al. found kills
  manual bookmarks. Prefer decorations and a command, not a view container.
- `editorMouseEvent` (weight 0.8, Table 1) has no clean analogue in the VS
  Code extension API — there is no mouse-move telemetry surface. Selection
  and cursor events absorb most of what it was standing in for; this is a
  real implementation gap, not a design choice.

**Genuinely new, untested in either era:** using this signal as LLM context.
Neither Weinig (2020) nor Ertli (2021) tested feeding DOI-scored fragments to
a model — there was no model to feed. That's the actual novel bet in this
plan, not a replication of the paper's own (null) hypothesis.

## 4. The feature

**Scope correction, 2026-07-28 (maintainer).** This is an *internal
algorithm* that makes the assistant better — not a navigation product. The
tracked attention data is an input to the LLM, and the two things the
developer actually touches are:

1. **Explain this, in context.** Select a snippet, right-click → a Contenox
   action. The selection goes to the model *together with* the session's
   attention profile, so the answer is grounded in where this developer has
   actually been working rather than in the file alone. This is the primary
   surface.
2. **What's hot, explained.** A rendered view of the most read/active
   sections with a short LLM-written line on each — "what am I deep in right
   now, and what is it doing" — rather than a bare ranked list the developer
   has to interpret.

Navigation aid (jump-to-focus) drops to a by-product. The paper's null result
was on navigation, so leading with navigation would be leading with the part
that tested null; leading with model grounding is the actual bet.

**What the developer sees.** Lines actively worked in — selected, edited, or
dwelt on across files this session — get a subtle gutter mark once their
score crosses the "relevant" band, as in the paper's tiering (p.7), rendered
as a VS Code gutter decoration. No new panel for the marks themselves. The
"what's hot" view is a webview or tree, populated on demand, not a standing
watcher.

**What it does automatically.** A pure, VS Code-API-free scoring module
(easy to unit test) accumulates a per-line-range score from editor events,
decays it over inactivity, and applies distance-based masking so one
function doesn't light up every line — the same three moves as the paper's
algorithm (weighted events, decay, masking; pp.8–11), re-derived for VS Code
primitives instead of Mimesis ones.

**Where it lives.** `packages/vscode/src/editor/` (alongside the existing
`context.ts`), as a new tracker module plus a decoration renderer. No new
activity-bar view, no new webview.

**VS Code APIs required (verified against the current API reference):**

- `vscode.window.onDidChangeActiveTextEditor` — file-switch signal (≈ paper's
  `viewEvent`).
- `vscode.window.onDidChangeTextEditorSelection` — cursor/selection signal
  (≈ `editorTextCursorEvent` / `textSelectionEvent`).
- `vscode.window.onDidChangeTextEditorVisibleRanges` — scroll signal
  (≈ `scrollEvent`).
- `vscode.workspace.onDidChangeTextDocument` — edit signal (≈ `codeChangeEvent`).
- `vscode.window.createTextEditorDecorationType` — the gutter mark itself
  (`gutterIconPath`, `overviewRulerColor`, or a subtle background/border).
- `vscode.window.showQuickPick` — the jump-to-focus list.
- Optional, Phase 2+: `vscode.languages.registerCodeLensProvider` (an
  above-line "visited N×" annotation) and the `vscode.executeDocumentSymbolProvider`
  built-in command (to snap a hot line to its enclosing function for a nicer
  decoration range). Both real APIs, both add real per-keystroke
  invalidation cost — first things to cut if attention is tight.
- Confirmed **not available**: a public API to read VS Code's own
  back/forward navigation-history stack (checked against open feature
  requests — microsoft/vscode#136878, #291688 — and the extension API
  guidelines; none exists). This is the direct VS Code analogue of why
  Mimesis had to instrument Eclipse in the first place: there is still no
  free lunch for "what has the developer been looking at," only cleaner
  event primitives than raw Eclipse gave the original plugin.

## 5. How it feeds the model

This must ride the mechanism the VS Code plan already commits to, not invent
one. `docs/development/internal/vscode-implementation-plan.md` Phase 3
("Real context") already plans attachment chips built from ACP
`resource_link` content blocks (`libacp/content.go:55`,
`NewResourceLink(uri, name) ContentBlock{Type: "resource_link", URI, Name}`),
replacing today's TTL-bound `pendingContext` side-channel
(`packages/vscode/src/chat/ChatWebviewViewProvider.ts:36,42`) and the
`EditorContextAttachment` producer in `packages/vscode/src/editor/context.ts`
(kinds: `selection`, `active_file`, `diagnostics`).

Wellworn adds one more producer function to that same family: at prompt
time, it turns the current top-K scored fragments into `resource_link`
entries — `name` carrying something like `wellworn: path:12-31 (score 47)`
so the model can tell this is ambient orientation context, distinct from
something the user deliberately attached. It attaches alongside, not instead
of, explicit attachments; it is visible and removable like every other
Phase-3 chip (no TTL, per that plan's own design). No new wire format, no new
ACP method, no `_contenox/*` extension needed — the existing content-block
type and the existing chip mechanism carry it.

This is a genuinely different signal from `workspace_search` /
`contenox index` (`docs/integrations/tools/local.md:505-530`,
`internal/services/workspaceindex/`): that tool answers "where in the whole
repo is X," from embeddings, on demand. Wellworn answers "where has *this
session's own attention* actually been," from live interaction telemetry,
pushed proactively. They're complementary, both riding `resource_link`, and
should not be collapsed into one mechanism.

Because scoring and attachment production are entirely client-side
(extension-host state, computed before anything reaches the ACP wire), this
needs **zero changes under `internal/kernel/` or `internal/services/`**. That
absence is worth stating plainly, given the standing constraint in the
implementation plan (§7): if this design had needed a kernel change, that
would have been the signal to re-derive it, not build it.

## 6. Already exists vs. new work

**Already exists, reusable as-is:**

- ACP `resource_link` content blocks — `libacp/content.go:12,55` — the exact
  vehicle for step 5, no new type needed.
- The `_contenox/*` extension-method mechanism
  (`libacp/methods.go:49-76`, `SetExtRequestHandler`/`SetExtNotificationHandler`
  on `libacp/conn.go:177-190`, wired in
  `internal/surfaces/acpsvc/transport.go:295`, with production precedent in
  `_contenox/autocomplete` and `_contenox/terminal/run`) — not needed here
  since scoring stays client-side, but confirms the pattern exists if a
  future need (e.g. persisting scores server-side) ever arose. It shouldn't:
  see §8, not doing.
- `workspace_search` / `contenox index` / `contenox search`
  (`docs/integrations/tools/local.md:505-530`,
  `internal/services/workspaceindex/*`, `internal/services/searchtool/searchtool.go`)
  — a complementary, unrelated retrieval mechanism, already emitting
  `path:startLine-endLine` citations and already the thing `resource_link`
  entries can point at. Confirmed it does not track per-session interest —
  its `Hit.Score` is plain cosine similarity, no recency/frequency weighting.
- `internal/services/missionchanges/missionchanges.go:20-57` — the closest
  existing "degree of interest" precedent in this codebase, but for a
  different subject (files changed in a mission's replay journal, additively
  weighted by edit/delete/move/read/execute) and explicitly "advisory only...
  scores rank and anomalies flag, they never gate." Worth following as a
  design precedent (rank, never gate) — not reusable code, wrong subject and
  wrong layer (kernel-side journal vs. client-side editor telemetry).

**Investigated and ruled out as a base:**

- `internal/surfaces/beamtui/comp/fileaddr/{fileaddr.go,browse.go,noise.go}` —
  a TUI `@`-mention file picker: static directory enumeration plus fuzzy
  filter, no interest tracking, no scoring, no decay. It answers "what files
  exist here," not "what have I been looking at." Correctly a false lead:
  it's beamtui-only and the maintainer's own reasoning for building this in
  VS Code is to use VS Code's editor APIs instead of reimplementing this kind
  of thing again.
- `internal/surfaces/contenoxcli/attachments.go` — CLI `--attach` image
  loader, unrelated to file/line bookmarking; only useful here as a reminder
  that "attachment" already has three separate shapes in this repo (CLI
  image flag, ACP `resource_link`, VS Code `EditorContextAttachment`) and
  this feature must ride the ACP one, not add a fourth.

**Genuinely new work:** the DOI/interest engine (event listeners → scored
line ranges → decay → masking, re-derived for VS Code's event set); the
gutter-decoration renderer; the jump-to-focus quick pick; the
`resource_link`-producing function that reads current scores at prompt time.
None of it has a direct antecedent in this repo.

## 7. The name

Three candidates, all checked against existing dev-tool names by search.

1. **Wellworn** — recommended. "The well-worn parts of your code" is
   immediately legible, felt-pain framing without needing the metaphor
   explained, and no colliding developer tool or extension found. Works as a
   command prefix: `contenox.wellworn.jumpToFocus`.
2. **Waymark** — the closest literal fit (a marker placed to guide a
   traveler along a path), but real collision: an existing GitHub project
   `waymark-sdk-tool` and an unrelated but established company, Waymark
   (video-ad tooling), both live in adjacent-enough space to cause confusion
   in search and support requests. Second choice, not recommended, for that
   reason alone.
3. **Furrow** — a groove worn into ground by repeated passage; equally clean
   collision check, equally apt metaphor, but more oblique for a global,
   partly non-native-English developer audience than "well-worn."

Recommendation stands: **Wellworn**, shipped as a feature of the Contenox
extension (`contenox.wellworn.*` commands), not a separate rebrand — the
maintainer asked for a feature of Contenox, not a new product.

## 8. Phased plan and "not doing"

Costed in maintainer attention, cheapest and most reversible first.

- **Phase 0 (prerequisite, not this feature's cost):** land
  vscode-implementation-plan.md Phase 3 (`resource_link` attachment chips).
  Wellworn's model-feed half rides that mechanism; it doesn't exist without
  it.
- **Phase 1 — DOI engine, ~1–2 days.** Event listeners, the scoring module
  with unit tests (a pure function, testable without a VS Code host), decay,
  masking. No UI. Validate on real sessions via a debug command that dumps
  current scores to an output channel before building anything visible.
- **Phase 2 — Gutter decorations + jump-to-focus, ~1 day.** The cheapest
  visible payoff: decorations are a repaint, not a new view container or
  webview.
- **Phase 3 — Model feed, ~half day, contingent on Phase 0.** The
  `resource_link` producer function, default-on but visible/removable like
  every other chip, with its token cost exposed the same way `usage_update` /
  `UsageCost` already exposes turn cost in the panel plan (implementation
  plan §2 table) — ship it observable, not silent.
- **Phase 4, optional and first to cut — CodeLens annotations.** Real
  per-keystroke invalidation cost for a marginal UI gain over the gutter
  mark.

**Not doing:**

- No standing "all bookmarks" panel or activity-bar view. That is exactly
  the friction shape Guzzi et al. found kills manual bookmarks, and a
  heavyweight UI surface a solo maintainer would then have to keep working.
- No cross-session or cross-machine persistence of scores. That would need
  storage outside `packages/vscode`, likely reaching into
  `internal/services/`, which the standing constraint flags as a red flag
  to re-derive, not a free choice.
- No reuse of `beamtui/comp/fileaddr` — wrong layer, wrong subject.
- No attempt to reconstruct a mouse-move signal — the API doesn't expose it,
  and cursor/selection events already absorb most of its intent.
- No re-litigating navigation count as a success metric for this feature.
  The paper falsified it as the right target once already (p.31); don't
  measure this feature's worth by developer click count either.

## 9. What would change my mind / unknowns

- The paper's own surviving insight (files-touched narrowing) is itself weak
  evidence — n=7 vs n=13, explicitly hedged by the author, and possibly a
  demand-characteristics artifact rather than a real tool effect (thread of
  validity #7, p.38). If early dogfooding of Phase 3 shows no measurable
  improvement in how well the model grounds itself (still asks "which file"
  as often, or the attached fragments go unused in its own reasoning), cut
  Phase 3 and keep Phase 1–2, which stand alone as a human-facing feature
  regardless of whether the model benefits.
- Whether `TextEditorDecorationType` repaint cost is noticeable across many
  open editors / very large files is unverified — cheap to test before
  widening Phase 2 rollout.
- Whether attaching wellworn context on every turn adds more token noise
  than it removes uncertainty is unverified. Ship it observable (cost
  visible via the same usage surface the panel already plans) and make
  default-off the fallback if users routinely strip the chip.
- The single biggest unproven assumption in this whole plan: that "where a
  human's attention recently was" correlates with "what's relevant to the
  current task" for an LLM the way the DOI/Mylar lineage assumes it does for
  a human's own working memory. Neither this paper nor anything in this repo
  has ever tested that transfer. Treat it as a bet, not a fact, until
  dogfooding says otherwise.
