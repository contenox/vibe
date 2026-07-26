# Pando mining report — whiteroom concepts (fleet, context, TUI)

Conceptual findings from a clean-room study of digiogithub/pando (2026-07, MIT,
OpenCode lineage — same family Crush grew from), mined against our fleet/
mission stack and the beam blueprint. Implementers build from THIS document;
the study clone was deleted. MIT means idea transfer carries no obligations;
we still never copy code, identifiers, or prose. Pando is also our named
anti-model for sprawl — gems here are mechanisms extracted from an organism
whose shape we deliberately avoid.

## Verdict vs. our architecture

Pando's delegation layer solves problems of *their* shape (multi-process
dispatch races over a shared store) that our single in-process fleet doesn't
have — skip their claim-lease/CAS machinery entirely. On durable-record-first
report handling and unforgeable unit identity, our stack is ahead. The gems
below are the mechanisms that fill holes we actually have.

## Ranked adoption queue (cross-front)

**1. Tool-output filter engine** (→ `internal/services/localtools`, M effort;
the top gem — the missing PREVENTIVE primitive in our context story,
complementing planned recovery/trim and per-step containment).
Ordered declarative filters matched by regex over the full command string;
first match wins; precedence project-local → user/global → embedded defaults.
Native structured parsers consulted first (go test -json, lint JSON, tsc
diagnostics) with 3-tier degradation: full structured decode (keep failures +
summary tally, drop passing bodies) → regex grep for failure signatures →
passthrough; parsers return a tier, never an error. Transform pipeline in
fixed order: strip ANSI → per-line regex subs → whole-output success-collapse
(with unless-guard) → drop-list XOR allow-list (declaring both = compile
error) → per-line length cap (rune-safe!) → head/tail windows with explicit
elision markers → absolute line cap → on-empty message. Structural guarantee:
only stdout is filtered — stderr, exit code, and failure suffix are assembled
after filtering and untouchable. Fail-safe everywhere: invalid filter skipped,
malformed file contributes its valid entries, no-match = raw, live kill
switch. Filter files carry inline test cases run by a validator verb (add
match-assertion cases — "command X must/must-not hit filter Y" — their gap).
Record chars-before/after via tracker so the compression headline is measured,
not inherited. OUR improvement over theirs: filtered inline result + raw
preserved in the existing spool with a notice naming filter and spool path
(they have one artifact; the human can never see what raw looked like).
Agent shell tool only; the user's `$`-line never enters context.

**2. Fleet admission cap with teaching refusal** (→ `fleetservice`, S).
Before allocating a unit, count open units (global + optionally per agent)
and refuse dispatch past a ceiling with a message naming the cap and value.
Explicit operator retry/relaunch may bypass; automatic dispatch may not.
Today nothing bounds fleet width (ComputeBounds caps one mission's spend
only). Their negative lesson: they displayed the cap in two UIs while
enforcing it nowhere — never expose a knob before it's enforced. Pair with
nil-safe atomic counters (dispatches, refusals) surfaced as one optional line.

**3. Streaming render trio** (→ transcript-view, S each; mechanisms for
requirements the blueprint states abstractly):
(a) debounced re-render: unfinished message repaints at most once per ~150ms;
each delta bumps a sequence and schedules a tick for the remaining window;
stale-sequence ticks dropped; final state renders immediately.
(b) per-message render cache keyed (width, content-signature) storing lines +
height; append evicts only previous-last + new; width change invalidates all;
cache the markdown renderer per width too (their hidden hotspot).
(c) streaming-safe segmentation: split prose vs fenced code tolerating
variable fence lengths and an UNTERMINATED fence at stream end (half-streamed
code already renders as code); language chips; bare URLs → OSC 8 hyperlinks.

**4. Conclusion-driven parent re-entry + await/join (the D28 answer
material)** (→ reportrouter extension + missiontools, M–L, opt-in per
envelope, never default). Re-entry: when a unit concludes and the supervisor
session is idle, start ONE parent turn whose prompt is the batched results +
explicit resume framing; guardrails: small per-session re-entry budget
resetting on human turns; debounced batching while siblings are outstanding;
if parent turned busy, degrade to mid-turn delivery; visible system framing.
Await/join: a non-blocking tool registering a join condition (all/any/N-of-M,
deadline ~1h default) whose response instructs the model to END its turn;
already-satisfied returns inline; completion re-entry suppressed until join
fires, framing names satisfied-vs-deadline. Terminal-failed units count as
reported. Re-entry is compute spend → govern via a new ComputeBounds field.

**5. Conclusion verification gate** (→ missiontools/missionservice, S).
On result reports naming artifacts: stat claimed paths against the unit's
workdir; positively-missing → downgrade success→partial + append a warning
naming what's missing; fail-open on unverifiable (URLs, prose); stat errors
other than not-exists count as present. Nothing is ever discarded. Our
Report.Refs (paths only) makes this nearly free.

**6. Session modes as composable prompt policies** (→ command-palette slot +
ACP config options, S–M; fills the blueprint's empty "session modes" slot).
A mode = per-session prompt-injection policy, not a code path (surface-
independent by construction). Fixed composition order when stacked (approach
→ documentation → scope → brevity); activation synchronous + local (toast,
no turn); FINISH runs a real closing turn (verify with real command output,
state what is NOT done) and clears only on success; explicit precedence
yielding to user instructions and permissions; brevity never compresses code,
paths, errors, test output, warnings; hard no-git-side-effects contract in
workflow modes. Their lesson: always show an active-mode status-bar chip.

**7. Unseen-report backfill cursor** (→ beam engine-bridge/mission-panel +
per-session KV, S–M; D21/D22 orbit). Durable fact first, broadcast as mere
wakeup: on every wakeup drain everything past a persisted watermark; ack
monotonically after handling; replay unacked backlog at startup; slow
safety-net tick; ack-without-acting past a staleness threshold (day-old
conclusions after downtime are noise, not recovery).

**Decision inputs recorded** (not work items): D20 attention-ask two-phase
multi-question form with per-question options + "other" free text; D27 nested
unit rendering under the firing card (child events invalidate parent card
cache; "N units running" counter); D43 auto-derived transparent theme
variants; D2/D34 field data — the copy chord is unreliable across terminals,
copy-on-selection-end + OSC 52 (tmux-wrapped) always; window-title running
indicator with title restore on exit; steering-inbox materialized only at
safe loop boundaries (if D28 resolves toward steering); named-argument custom
command templates (post-V1 palette registry). Deferred with pattern noted:
warm instance reuse (only the idle-GC two-sided close race-closure matters if
we ever reap idle instances); fail-closed peer capability negotiation (if a
remote story ever opens: two-sided opt-in, version ≥ check, absent fields
decode to refuse, all failures fold into graceful fallback).

**Sub-mission prerequisite bundle** (record for the day units may spawn):
propagated depth counter, outstanding-per-parent cap with instructional
refusal ("await or list before forking more"), re-entry budget. Today our
units structurally cannot spawn — no bomb possible.

## Anti-patterns (the sprawl bill, itemized)

Config knobs displayed but not enforced · known-unfixed shared-state race
left because the blast radius outgrew the fix · inverted boolean knobs to
work around config-library defaults · README fiction (the advertised
"vim-like editor" does not exist in the code — inherited marketing) · surface
multiplication (TUI+PWA+desktop+REST+unrestricted-shell-over-WebSocket whose
docs are mostly a security warning; 7 locale files per knob) · god root model
(~3,000 lines, ~18 boolean-flagged dialogs, no dialog stack) · one-artifact
filtered output (human can't see raw — our spool split must stay) ·
mouse-capture-everything (though their ANSI-stripped shadow-buffer selection
is the known-buildable fallback if alt-screen ever forces owned selection).
