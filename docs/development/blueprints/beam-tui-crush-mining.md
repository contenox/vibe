# beam — Crush mining report (whiteroom spec input)

Conceptual findings from a clean-room study of charmbracelet/crush (2026-07),
mined against the beam component blueprint (`beam-tui.md`). **Implementers
build from THIS document and the blueprint only — never open the Crush
repository.** Crush is licensed FSL-1.1-MIT (source-available, non-compete
clause, converts to MIT two years per release); copyright protects expression,
not ideas, so independently implementing the behaviors below from this
conceptual description does not rely on their license. Do not reuse their
code, identifiers, file structure, comments, or string literals. The study
clone was deleted after this report was written.

## Gems (ranked; each attaches to a blueprint component)

**G1 — Stable-prefix incremental markdown rendering** (transcript-view, L).
During streaming, cache the render of a provably safe prefix of the markdown
source and re-render only the tail per flush. A cut point is valid only after
a blank line where no construct can span it: even count of code-fence lines;
previous non-blank line is not a list item / table row / blockquote / indented
code / setext candidate; next non-blank line is not a setext underline; and no
list marker, HTML-block opener, or link-reference definition anywhere in the
prefix (those act at a distance). Glue fragments with trimmed margins + one
blank line. On any doubt — no boundary, width change, content not a
byte-prefix extension (retry/edit) — full re-render and reseed. Correctness
lives entirely in the conservative boundary predicate; the fallback makes
wrong output impossible, only slow frames. Apply independently to thinking
and answer streams.

**G2 — Layered render-cache stack + frozen finished items + batched resize
warming** (transcript-view, L). (a) Per-item monotonic version; (b) per-item
section caches keyed by content-hash + width (thinking/content/error cached
separately; length-prefixed hash framing); (c) list-level memo of (rendered
string, pre-split lines, height) keyed by width+version — items that report
finished are FROZEN and never re-rendered; (d) frame-level: decode the
composed frame into a cell buffer once, blit byte-identical frames. On
resize: reflow visible items immediately; after ~120ms settle, re-warm the
rest in ~25-item batches across frames, guarded by a sequence number so a new
resize cancels stale warming. Separate memo for per-line style-prefix
application (focus/gutter) so styling never re-renders content.

**G3 — Input grace period on async approval prompts** (approval-cards +
keymap-registry, S). When a keystroke-stealing card appears asynchronously,
absorb key input until the keyboard is quiet for a short window (with a max
delay) so an in-flight keystroke can never answer an approval. Exception: if
the same prompt type closed within a short window, skip the grace (burst
answering must stay fast). Adopt as an acceptance criterion.

**G4 — Prompt queueing + laddered Escape** (composer + engine-bridge, M).
Submitting while busy queues the prompt (count badge, expandable). Escape
ladder: queue present → first Escape clears queue; else first Escape arms
cancel-intent (visible, timed), second Escape cancels; timeout disarms.

**G5 — Empirical terminal-capability probing + tiered notifications**
(completion-notification + theme-styles, M). Probe at startup (timeboxed)
instead of trusting $TERM: color depth, graphics, cell size, and a
notification query round-trip (send the notification escape in query mode
with a self-id, parse the response). Backend ladder: native OS notifier when
local → modern escape-sequence notification → legacy urxvt style → BEL floor.
If the terminal cannot report focus, disable notifications entirely (without
focus events, suppression is impossible — honesty means silence). Bonus:
Windows Terminal taskbar-progress escapes while busy.

**G6 — Structured question forms as a blocking service** (approval-cards /
attention-ask, M–L). Beyond allow/deny: typed questions published through the
permission-flow shape (publish request, block tool until resolved). Kinds:
yes/no, confirm, single/multi choice, free text, open-editor-for-long-answer;
batch several into one tabbed form with final confirm; every answer may carry
free-text notes. One pending question at a time (the tool blocks) —
eliminates correlation bookkeeping.

**G7 — Deterministic pre-rendered spinner animation** (liveness +
test-harness, M). Pre-render all animation frames to styled strings at
construction; the tick loop only indexes arrays. Seed all randomness from a
hash of (settings, identity) so output is a pure function of (settings, id,
tick) — byte-identical across processes, hence golden-testable. Tick messages
carry the owning spinner's id (no cross-advance). Elapsed time via pull-based
suffix callback; ellipsis animation yields to the timer suffix. ~20fps main
cadence, slower ellipsis cadence.

**G8 — Paste intelligence** (composer + selection-clipboard, M). Classify
bracketed paste before insertion: normalize CRLF; oversized paste becomes a
named synthetic attachment (paste_N.txt, MIME-sniffed, size-capped with
warning) instead of entering the buffer; a paste that parses as existing
image-file paths auto-converts to image attachments; else insert literally.
Clipboard image read falls back to interpreting clipboard text as a path.

**G9 — One workspace seam, two transports** (engine-bridge, S now / L later).
All UI talks to one workspace-shaped interface with interchangeable
implementations: in-process, or an HTTP client to a self-spawned daemon on a
unix socket — version handshake (mismatch → shut down + respawn), stale
socket detection, single-flight spawn lock. Rule for beam: keep engine-bridge
strictly transport-shaped (no shared-pointer conveniences) so
turns-survive-process-death becomes a deployment flag, not a rewrite.

**G10 — Thinking as bounded tail window + duration footer** (transcript-view,
S). Live reasoning renders in a quiet box showing the last N lines with an
"… X earlier lines" hint; per-message cycle collapsed → larger tail → full.
Slice AFTER markdown rendering so fences/lists in the tail render correctly.
On completion: collapse to a one-line "Thought for 12s" footer.

## Confirmations (Crush independently matches the blueprint)

Two-stage Escape cancel with visible armed state · follow-mode scrolling
(scroll up breaks follow, bottom re-arms) · single toast primitive with
severity+TTL through the status bar · session picker as filterable overlay
with inline rename and two-keystroke delete · $EDITOR composition via
tempfile + suspend + reread (they even pass the cursor position) · prompt
history seeded from persisted user messages incl. shell-passthrough lines ·
allow/allow-for-session/deny where the session grant is recorded in the
permission SERVICE race-safely (never UI-side memory — the D19 middle option
is possible without violating the invariant) · semantic style registry
(components never construct colors; theme switch rebuilds once) · goldens
confined to the leaf diff renderer at many sizes, everything else headless
unit tests, no PTY · onboarding as a state machine gating the chat surface ·
all ANSI string surgery through one width/truncate helper · diff: unified +
split, syntax highlight, gutters, horizontal scroll · modal dialogs as a
stack (close-by-id, close-topmost) · compact layout below ~120 cols.

## Anti-patterns (deliberately avoid)

- **God root model** (~4,850-line single Update). Beam's 21-component split +
  keymap-registry exist to prevent exactly this. Keep their nuance though:
  child components as plain stateful structs with imperative methods — not
  nested Elm models. Compose explicitly; just don't centralize behavior.
- **Mouse capture + hand-rolled selection**: hundreds of lines of drag state,
  line/col math, input coalescing, and cache-unfreezing during drags — the
  cost beam's zero-mouse-capture decision (D2) avoids. Ratify D2.
- **Global singleton turn timer**: only one timed activity at a time — beam
  has concurrent activities by design; keep per-activity clocks (liveness).
- **Synchronous service probes in render paths**, later patched with TTL
  caches + generation counters. Beam's rule stands: views render only
  published state from the event stream.
- **Modal takeover approvals**: import the card content ideas (per-tool
  headers, diff toggle, one-key options, G3 grace) — never the takeover.
