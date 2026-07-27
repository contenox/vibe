# Tool hardening — local tools vs. model diversity

Date: 2026-07-21
Status: blueprint (research landed; implementation staged). Companion incident:
the first real fleet dispatch derailed because `list_dir` did not distinguish a
50MB executable from a directory (fixed same day in `internal/services/localtools/fs.go`
— suffixes, binary sniff, teaching errors). The general problem stands: the
local tools work well with Gemini and degrade on other models, and everything
in the product builds on this layer.

Sources: mined 2026-07-21 from openai/codex, google-gemini/gemini-cli,
sst/opencode, charmbracelet/crush, OpenHands + OpenHands/software-agent-sdk,
Aider-AI/aider, claude-task-master, zed-industries/claude-code-acp. File-level
attributions live in the session research report; the load-bearing findings
are restated here so they survive without it.

## The structural finding

**Every mature project converged on the same conclusion: the tool surface is
not one fixed thing — it is selected, tuned, or repaired per model.**

- aider: `model-settings.yml`, ~150 entries — edit format, reminder placement,
  anti-laziness/anti-overeager flags, reasoning-tag stripping, per model, each
  flag A/B-tested against their benchmark with the deltas left as comments.
  Unknown models fall back to the *safest* format (`whole`), not the best one.
- gemini-cli: whole tool manifests forked by model family; the verbose,
  example-laden descriptions are the DEFAULT for non-flagship models; terse
  MUST-laden phrasing is reserved for the one model they trust most.
- codex: five per-model system-prompt files; `apply_patch` is a
  grammar-constrained freeform tool "well-suited for GPT-5 models".
- opencode: swaps the entire edit tool for a patch-envelope tool on exactly
  the GPT models RL-trained on that envelope.
- OpenHands SDK: `model_features.py` capability registry (substring-matched,
  last rule wins) gating stop-words, string serialization, image encoding,
  prompt-cache behavior — one table, one row per family, reasons inline.
- aider tried native function-calling for edits and **abandoned it**
  (`EditBlockFunctionCoder` raises "Deprecated") — native tool-calling is not
  automatically the more-portable choice.

Consequence for contenox: a `ModelProfile` table (exact-match then substring
fallback) is the structural precondition for every other hardening step. The
current Gemini-tuned surface becomes the *special case*; the verbose default
serves everyone else.

## The ten recommendations (priority order)

1. **Per-model capability table** (`ModelProfile{tool_verbosity,
   schema_strictness, needs_examples_in_system_msg, …}`) — the precondition.
2. **Description verbosity as a per-model dial, defaulting to verbose** —
   gemini-cli's own the non-flagship default.
3. **Complete the `list_dir` family against the OpenHands reference**: size
   checked and reported *before* content sniffing; content-sniff (not
   extension) binary detection with image carve-outs; size + executable bit on
   every entry. (Largely done 2026-07-21; check ordering + carve-outs.)
4. **Never truncate silently: spool full output to disk and name the exact
   retrieval step** — "use start_line: 2001", a concrete number, not advice.
   (gemini-cli 20%-head/80%-tail; opencode `.opencode/tool_output/` spool;
   OpenHands names the resume line.)
5. **Fatal-vs-recoverable severity bit on every structured tool error** —
   gemini-cli marks only disk-full fatal; boundary/permission errors are
   documented as recoverable-by-correction. Changes retry behavior uniformly.
6. **Malformed-call repair layers — several narrow and model-attributed, never
   one generic-lenient one**: control-char sanitizing, chunked-string-array
   rejoining, trailing-garbage truncation, a field-name typo table (OpenHands
   `fix_malformed_tool_arguments`); scoped and disable-able like gemini-cli's
   `unescapeStringForGeminiBug` (explicitly off outside its narrow case).
7. **"Did you mean" on every match failure** — sibling-filename fuzzy match on
   missing files (opencode), best-window similar-lines on edit no-match
   (aider). Cheap and model-agnostic.
8. **Non-native function-calling text fallback** for models with flaky native
   calling — OpenHands' `<function=name><parameter=…>` protocol + self-healing
   parser + tool-name aliases, few-shots skipped for models that know it.
9. **Elision-marker detection as a universal guard** — `# rest of code…`,
   `// unchanged`, bare `…` in write/edit payloads rejected before the write.
   Independently guarded in gemini-cli, aider, OpenHands: universal, not
   model-specific.
10. **Malformed-tool-call rate as a first-class per-model metric** — aider
    publishes `percent_cases_well_formed` beside pass rate. This is the signal
    that would have caught "fine on Gemini, degrades elsewhere" before a live
    derailment.

**The fuzzy law — fuzzy to suggest, exact to act.** The field splits on
fuzzy matching in tools: one school applies edits through tolerant matcher
cascades (opencode's exact→trimmed→normalized chain, Cline/Roo's similarity
thresholds, gemini-cli's LLM edit-corrector); the other (Claude Code's Edit,
OpenHands' file_editor, aider's apply step) refuses fuzzy application —
exact or loud failure, with fuzz demoted to the error message ("did you
mean…"). The evidence favors the second school: opencode had to guard its own
mechanism against over-grabbing ("matched span much larger than oldString"),
and a silently misplaced edit is corruption. Binding here: contenox tools may
search fuzzily and TELL (rec 7); they must never fuzzily DO. Fuzzy-to-select
in UIs (palettes, pickers) is separate and unconstrained.

Error-string craft, distilled from the best found: name the exact next action
("narrow with dir_path or include_pattern"), enumerate what IS available on
not-found, echo the offending input back truncated so a retry has grounding,
prefix errors with a named class (`UnifiedDiffNoMatch:`) so models and humans
can grep failure kinds, and give remediation recipes for detected misuse (the
OpenHands "you put a Python literal in the shell command — write a script with
file_editor or use a heredoc" error is the reference).

## The eval harness (aider-benchmark-shaped, incident-driven)

> **Retired 2026-07-27:** the `internal/tooleval` implementation of this
> harness was removed with the V1 chop (zero production importers). The design
> below stays as the record; the hardening recommendations themselves live on
> in `internal/services/localtools` (Rec 4/5/7 references in code).

```
scenarios/<tool-family>/<slug>/
  instruction.md   # literal task given to the model
  fixture/         # on-disk state: the 50MB executable, an escaping symlink,
                   # non-UTF8 files — real incident shapes
  verify.(go|sh)   # asserts the INVARIANT (file never written, task done),
                   # never an exact tool-call sequence — models differ in path
  meta.json        # {tool_family, required: bool, max_iterations, tags}
```

- Required scenarios gate; optional ones are named after real incidents
  (yesterday's derailment becomes scenario one, not a one-off patch) —
  OpenHands' `b*` behavior tests forbid artificial setups for this reason.
- Drive the real tool pipeline in-process; replayed-response mode for
  determinism, live-model mode for the matrix; iteration caps + runaway
  stopper.
- Two independent axes per model×scenario: task success AND first-attempt
  tool-format compliance. Published as a matrix (Gemini baseline, one Claude,
  one GPT-class, one weak/local model) so "works on Gemini, breaks on X" is a
  specific red cell.
- Share fixtures with the existing localtools unit tests — one ground truth.

## Staging

Done (2026-07-21): rec 3's core (`fs.go` type-awareness). Next slices, in
order: 4 (truncate-and-spool), 5 (severity bit), 7 (did-you-mean) — all
localtools-contained; then 1+2 (the ModelProfile table — coordinate with
llmrepo/modelrepo owners); then the harness (whose first scenarios are the
incidents already on record); 6/8/9 ride behind the table. Rec 10 falls out of
the harness.
