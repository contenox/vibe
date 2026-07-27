# TODO — contenox

**Direction:** contenox is an open coding harness — a terminal CLI, ACP
editor integrations, and the beam TUI as thin surfaces over the kernel (see
WHY.md). **Envelopes are the identity**: the approval-policy files that bound
what an agent may do unattended are what make contenox contenox — work that
makes envelopes more real (surviving restarts, enforced limits, verified
results) outranks cosmetic work. **V1.x tailoring (2026-07-27):** feature
work weights toward Go and TS/JS development and the supervisory PM loop
(fire → track → report → review → conclude).

Standing rules: business logic lives in services, surfaces stay thin · from
other projects we take individual mechanisms, never their product shape ·
anything the CLI/docs advertise must actually work · a tool never requires a
subprocess sidecar to exist — MCP is supported natively, so integrations are
in-process pure Go, MCP, OpenAPI, or commands the agent itself runs in the
sandboxed shell.

## Shipped ledger

**2026-07-27 — tools, retrieval, and the telemetry rule** (staged, uncommitted):
- **Four toolsets shipped, all always-on and envelope-addressed**: `git`
  (10 per-op tools over go-git, no network ops, reads allow / mutations
  approve) · `gointel` (in-process Go intelligence over x/tools — describe,
  definition, references, implementations, symbols, diagnostics; names-first
  queries, snapshot+mtime freshness; warm queries ~70µs, whole-module
  references ~4ms) · `goja` (`goja_eval` + operator-authored script tools
  from `$CONTENOX_DIR/tools/*.js`; one boundary rule — a script's only reach
  is `host.tool`, through the same HITL wrapper a model call meets) · `jq`
  (gojq; allow-tier *by construction* — its deadline bounds recursion AND
  compute, RE2 regexes, no catastrophic backtracking).
- **Workspace semantic search**: `contenox index` / `contenox search` +
  `workspace_search`, over the existing seams only (llmrepo.Embed,
  runtimetypes/libdbexec, the vfs+gitignore matcher, the mission/inbox CLI
  shape). FTS5 prefilter + cosine rank, create-once index generations,
  per-hit staleness marking. Measured on this repo: 7,196 chunks, ~40min at
  ~3 embed calls/s (ollama-bound), 49MiB peak RSS, warm queries 0.06–0.25s,
  incremental refresh 49x faster. Plan-then-confirm shows the bill before
  spending.
- **Shell policy became STRUCTURAL** (phase A of `shell-structural.md`,
  mvdan.cc/sh parser only): compound allowlisted lines stop interrupting
  (`git status && go build` → allow), and a **pre-existing hole closed** —
  `git status\nmkfs /dev/sda` was ALLOWED because `strings.Fields` ate the
  newline and matched the `git status` prefix. Literal-words rule,
  assignment-prefix rule, powershell guard, cleared-node audit shipping
  beside the code as the security argument.
- **slog eliminated as an API**: the tracker is the only instrumentation
  seam (the reason is `libtracker/redact.go` — tracker values are redacted,
  raw slog bypasses it and writes credentials verbatim). 42 sites across
  services/kernel/surfaces converted or correctly reclassified as
  user-facing output; slog survives ONLY as the tracker's sink in libtracker
  plus three individually-justified composition-root files, enforced by
  `TestUnit_NoDirectSlogOutsideSinks`.
- **Envelope deployment fixed**: presets now upgrade when provably untouched,
  and an install whose envelope predates a toolset is DETECTED semantically
  (which `tools:` values it never mentions) and told — by doctor and by one
  muted beam startup line — with `contenox init --refresh-policies`. An
  operator's envelope is never overwritten; it is a security boundary, not a
  cache.
- **Security findings closed** (each found by live use, none by unit tests):
  escape/bidi injection reaching the terminal through agent text and
  approval diffs · a cache stub rendered as a diff's before-side (and the
  gate's own read satisfying the read-before-write rule for the write it was
  gating) · `env`/`$ENV` reachable from a jq filter · goja's deadline burning
  on human approval time, which made script tools dead on arrival.
- **Blueprints**: `gointel.md`, `goja-tools.md`, `workspace-index.md`,
  `shell-structural.md` (settled, phase A built) · `yaegi-tools.md` (opened
  for co-authorship, NOT built) · mining reports `ee-mining.md` and
  `direktiv-mining.md`.

**2026-07-26 — the V1 reshape** (commits `b81624f3`+`c06751f9`, history
purged of accidental binaries 197→13MB, force-pushed, repo renamed
contenox/beam): killed the web UI, HTTP API layer + framework, OpenAPI
generator, modeld daemon + client, VS Code extension, UI library,
server-dependent CLI verbs, five unused libraries · layered restructure
(`internal/{kernel,models,services,store,surfaces}`, module
`github.com/contenox/beam`) · Make→Task · harness repositioning (copy EN+DE,
one Retired-R&D docs page, media on S3) · WHY.md · beam TUI component
blueprint + Crush/pando whiteroom mining reports + the eino decision record
(LEARN; revisit trigger written down) · image input end-to-end (ACP image
blocks, `chat --attach`) · `beam` reserved as a teaching stub.

**2026-07-27 — phases A–C** (commits `d0ec692a`+`7ed3f252`; providers
trimmed to ollama, vllm, openai, anthropic (kept but expensive — no paid
tests), gemini, bedrock, vertex):
- **Streaming truth**: providers emit raw deltas only; ONE engine-side
  assembler; typed terminal parcels (finish reason + usage); per-kind event
  subscription so observing can never fork execution; gemini stream
  system-prompt fix; in-stream errors surfaced; golden wire fixtures for all
  7 providers.
- **Capability truth**: declared models keep observed vision/thinking;
  vertex embeddings implemented; bedrock's false CanEmbed removed; doctor
  shows a vision summary; pinning a non-vision model with an image attached
  now teach-refuses instead of silently swapping models.
- **Provider correctness**: typed context-overflow/rate-limit sentinels per
  provider; OpenAI Responses reasoning parsed from the right field (+
  store:false); anthropic thinking blocks round-trip so thinking+tools
  multi-turn stops 400ing; vllm auth token flows; one shared HTTP client
  with timeouts + one Retry-After-honoring retry helper; bedrock
  inference-profile resolution + Think support; vllm tool names sanitized.
- **Cache utilization** (design: `provider-kv-cache.md`): session-sticky
  model/backend resolution; prefix determinism (day-granular {{now}} in
  system prompts, canonical tool order, chunked history trim — which also
  fixed a real bug: the old trim silently dropped the AGENTS.md message
  forever); cache-aware usage extraction with a strict normalization rule;
  anthropic cache_control breakpoints (with the API-required thinking-block
  skip), bedrock cachePoints, openai prompt_cache_key, vllm provenance
  wire-leak sealed.
- **Chain & envelope vet**: load-time validation with teaching errors for
  chains AND envelope files; `contenox vet`; sticky disable on broken rows;
  caught a genuinely stale file in the maintainer's own ~/.contenox.
- **Engine event contract**: `docs/development/engine-events.md` is
  normative, with drift-failing contract tests; journaled stream-end
  brackets; hierarchical addresses (chain/task/tool-call) on events and
  captured state.
- **Durable envelopes (the identity slice)**: approvals park ≤30s then
  checkpoint-and-release; versioned checkpoint with migration engine and a
  reflection guard against silent field loss; claim CAS picks exactly one
  resumer; PROVEN: suspend in process A, kill it, respond in process B →
  chain completes exactly once (deny variant too).
- **Mission surfaces**: `contenox mission list/show/reports/fire --wait`
  (in-process, honest semantics, real 5s e2e); fleet composition extracted
  from the CLI surface into `fleetservice.BuildInProcess`.
- **Fleet guards**: admission cap enforced-from-birth (`fleet-max-parallel`,
  teaching refusal); conclusion verification gate (hallucinated "done, see
  file X" arrives downgraded with a warning); nil-safe counters in doctor.
- **Context prevention**: tool-output filter engine (structured parsers for
  go test/lint/tsc, declarative fallback, stderr/exit untouchable, raw
  always in spool; measured: a 95KB go-test transcript → 430 bytes inline
  with every failure intact); `contenox tools filter test` validator.
- **Vision proven live**: a real image through `--attach` → capability
  routing picked the vision model → "I see a red circle."
- **Brand**: beam gold (light-beam amber ramp) across website tokens, CTAs,
  sidebar, and a scheme-aware SVG favicon (fixes the washed-out light-tab
  case); TUI accent ladder + usage rules recorded in the blueprint.
- **libbus**: Request timeout classification made position-independent
  (deadline expiring before/during insert now labels as ErrRequestTimeout)
  — was the full suite's only failure, a pre-existing gap. [staged, rides
  the next commit]

**2026-07-27 — the V1 chop (pre-beam cleanup)** [staged]:
- **Orphans deleted**: internal/tooleval (1,853 LOC + 23 scenario files, zero
  production importers) + its Taskfile task; scripts/demos/ (documented
  retired surfaces, pre-rename tape paths — clears the contenox/runtime grep);
  contenox-agentic-bench.ps1 + companion chain (drove retired modeld);
  internal/services/archiveutil (bonus find during the modeld grep: both its
  callers died in the reshape, zero importers).
- **Blueprint cull**: killed v1-feature-map, local-coding-node-goals,
  product-surface-truth (rule lives in this file's standing rules),
  agent-servers-and-client-e2e, windows/ (live content folded into
  windows-development.md). tool-hardening.md KEPT after the dangling-ref grep
  proved localtools code/comments cite its Rec 4/5/7 vocabulary — reclassified
  as decision record with a retirement note on the eval-harness section.
  acp-client-engine.md KEPT (client-hub capability, not superseded by
  engine-events.md). Indexes updated; agent-sandbox.md added to the acp index
  it was missing from.
- **statetype → runtimestate merge**: single ~100-line file, 14 import sites
  across 5 packages, zero symbol collisions; split had served the dead HTTP
  API.
- **Test truth**: runtimetypes.SetupStore (and the acpsvc/messagestore test
  setups the first sizing missed) now run SQLite — the engine the product
  ships — instead of a postgres:17 testcontainer; the Postgres manager,
  schema.sql, and localpostgres.go deleted (zero non-test consumers);
  suite went from container-bound to 16s. TestSystem_Ollama* short-gated.
  The two acpsvc flaky-under-load tests diagnosed: fixed 3s/5s notification
  drains racing spawned-subprocess startup — deadlines raised to 30s.
- **Hygiene**: modeld/openvino/llamacpp comment mentions fixed (acpsvc,
  llmrepo doc-comment, taskengine, runtimestate; cli.go's reserved-verb list
  stays on purpose); make→task in acp-client.md + windows-development.md;
  SUPPORT.md/issue templates verified already clean; full -short suite green.

**2026-07-27 — envelope core (the identity slice, part 2)** [staged]:
- **Resume hook registered at the composition roots**: acp_cmd injects its
  one durable-ask service into the engine (`enginesvc.Config.HITLService` —
  collapsing the old two-instance split that couldn't wake each other's
  waiters) and registers `agentservice.ResumeHook`; BuildEngine (chat/run/
  doctor/approvals) builds + injects + registers the same way. The
  process-independent resume entry now has production callers.
- **`contenox approvals list/respond`**: the durable ask inbox. `list` runs
  SweepExpired as its production tick (it had zero callers) and renders
  permission asks and questions in one table; `respond`
  (--approve/--deny/--answer) records the verdict and RESUMES the suspended
  run synchronously in this process, with teaching errors for every sentinel
  and honest degradation when no engine can be built. BuildEngine registers
  durable-only missiontools so a resumed MISSION chain's later
  report/finish calls land instead of failing as unknown tools.
- **`contenox mission stop`**: service-first `fleetservice.StopMission` —
  guarded abandon transition, new `hitlservice.AbandonMissionAsks` (closes
  the mission's pending asks hook-LESS; nothing of a stopped mission may
  resume), checkpoints deleted; live teardown via a StatusChanged bus
  subscriber in BuildInProcess (the subject had zero subscribers) so a stop
  from any terminal reaps the unit wherever it runs.
- **Dual-inbox wart fixed**: `RequestApproval` now adopts a caller-supplied
  ToolCallID as the durable row identity (adopt pending / return terminal
  verdict / create-with-ID + race re-read) — one row per ask, never a twin.
  Bonus: the wait now polls the durable row like the attention path, so a
  cross-process respond ends a parked wait instead of hanging to the ceiling.
- **Mission attention detach (questions now checkpoint)**: suspendable-call
  marker at the one execution site whose output satisfies suspendRun;
  `WithAttentionAnswers` (TEXT twin of approval verdicts); AttentionRequest
  gains AskID + ParkWindow with a typed pending error; execAskAttention
  consumes injected answers on resume, parks 30s then releases;
  Answer/AnswerAsAgent got the same waiterless resume-hook treatment as
  Respond; ResumeFromCheckpoint injects text for questions and restores the
  MISSION BINDING from the ask row's attribution (found live: without it the
  resumed chain couldn't see the mission tools at all). PROVEN by
  attention_resume_e2e: ask in process A, kill it, answer in process B → the
  operator's words are the tool result exactly once; refuse variant files
  the durable blocker.
- **Max-tokens truth**: `FinishReason` now rides ChatResult →
  ChatHistory → CapturedStateUnit → InferStopReason, populated by all 7
  providers (openai chat + Responses incomplete_details, anthropic
  stop_reason, gemini/vertex candidates, bedrock StopReason, vllm, ollama) —
  and vllm/ollama non-streaming "length" became a truncated SUCCESS
  (content preserved) matching the streaming assembler's contract instead
  of discarding the partial response behind an opaque error. A truncated
  success now reaches clients as StopMaxTokens; the beam TUI can warn.

**2026-07-27 — deferrals cleared + envelope copy** [staged]:
- **Cache deferrals landed**: StableHistoryLen now produced at the one
  request-build site in taskexec (all-but-last-message asserted stable; a
  trim this call asserts nothing — expected-cold per the design), so
  anthropic BP3 history breakpoints finally fire; ollama keep_alive set on
  every model-loading request (chat/stream/generate/embed; default 10m,
  `CONTENOX_OLLAMA_KEEP_ALIVE` overrides) — one un-annotated path would
  have silently reset residency to the 5m server default; PromptExecute now
  reports provider usage (interface widened through all 7 providers,
  Meta.Usage, TokenUsage event at the prompt call site). gemini explicit
  cachedContents stays deferred behind its own usage-evidence gate.
- **Vision follow-ups**: audit found all 7 providers already carry image
  wire tests (the mock e2e half was done); added the cost-gated real-model
  test `TestSystem_Ollama_Vision` (container + moondream, red-circle PNG,
  answer must contain "red"). Remaining manual: the ACP-editor-side sanity
  check; composer paste rides the TUI.
- **Envelope copy pass**: landing EN+DE positioning now names the envelope
  as the concept ("built around the envelope: the file that bounds what an
  agent may do unattended; missions are what envelopes make safe"), first
  capability card states the shipped fact (envelopes survive restarts —
  checkpoint, answer from any terminal, resumes exactly once); README intro
  reshaped the same way incl. `contenox approvals respond`. German mirrors
  EN; banned-phrase grep clean; website builds.

**2026-07-27 — beam ships (the V1 TUI slice)** [staged]:
- **No-framework architecture**: the blueprint's earlier bubbletea/lipgloss
  line is superseded — a schema/engine split instead (`frame` is pure-data
  Lines a component renders as a function of state+width; `term` is the one
  package touching the real terminal, a single-writer live-region renderer
  with guaranteed-restore raw mode; `style` is the sole StyleID→SGR table).
  Package list in one breath, all under `internal/surfaces/beamtui/`:
  `frame`, `textwidth`, `input`, `term`, `style`, `sanitize`, `keymap`,
  `liveness`, `enginebridge`, `testkit`,
  `comp/{brand,transcript,composer,statusbar,palette,approval,picker,
  fileaddr}`, `app` — plus the extracted `internal/surfaces/fleetboot`
  (the shared in-process fleet constructor `acp_cmd.go` and `beam_cmd.go`
  both call). Wired from `contenoxcli/beam_cmd.go`, beam's composition root.
  Full accounting of what shipped vs. what the blueprint still lists as
  deferred: `beam-tui.md` section 9 ("As built").
- **Constitutional verifications passed live**: the copy/paste acceptance
  tests (blueprint section 1) run by hand against a real PTY this session —
  copy-out of a long unwrapped code line came back unbroken, pasting 50
  lines landed as one composer block (never executed line-by-line), a clean
  exit restored cooked mode/cursor with no alt-screen residue, and a real
  turn showed streaming text, the activity spinner plus context gauge, and
  a resize without corruption.
- **Sanitize/security hardening**: `sanitize` closes ANSI/C0 escape
  injection and the Unicode bidi trojan-source class (U+202A–U+202E,
  U+2066–U+2069) at the one ingest boundary, before any untrusted text —
  tool titles, file names, session names, diff lines, agent output — ever
  becomes a drawn span.
- **Brand device**: the welcome header rasterizes the website logo-mark
  from its own SVG path geometry as half-block art, printed once into
  scrollback so it survives in screenshots and history; the status bar's
  identity segment is the persistent gold beam-bar `▌` + `contenox`, muted,
  never animated.
- **Review fan-out**: three adversarial reviews ran across the slice; every
  majority-severity finding fixed before this ledger entry.

## 1. Defects & doc-truth (2026-07-27 five-agent audit — fix first)

- [ ] **gointel freshness rides the backstop alone**: `Index.Invalidate()`
      is the documented primary freshness path with ZERO production callers
      (`gointel/loader.go:696`; only tests call it). Wire it into the
      `local_fs` write path.
- [ ] **AGENTS.md never reaches the fleet**: only `chat_cmd.go:142` and
      `run_cmd.go:284` call `agentsmd.Load`; ACP sessions and every
      dispatched mission unit build `PromptRequest` without it
      (`acpsvc/prompt.go:262`), and `contextasm.KindRepoRules` has zero
      consumers. Unattended workers run convention-blind — worst possible
      place for it.
- [ ] **The shell scrub is orphaned — nothing applies it** (found while
      fixing the "mission units bypass the sandbox" doc-truth item; docs
      corrected 2026-07-27, the mechanism gap is real and bigger). The wall
      (`libsandbox.Command`) has exactly ONE caller —
      `agenthost/externalacp.go:274`, foreign agents only. Contenox's own
      chains (`chat`, `run`, `beam`, `acp`, dispatched mission units) run
      `local_shell` and the `shell_session` PTY as plain children of the
      runtime, and the environment scrub they were documented to get died
      with `serve_cmd.go` in 7ffadb05: `WithLocalExecScrubEnv`,
      `shellsession.Config.ScrubEnv`, `newLiveGlobalShellEnv` and
      `resolveSandboxScrubs`'s injector now have ZERO production callers.
      So `SANDBOX_SHELL_SCRUB`, `SANDBOX_ENV_ALLOW/DENY` and
      `contenox shell-env set` govern nothing while `contenox sandbox env`
      still previews the policy — the CLI advertising what does not work.
      Re-wire at the surviving spawn sites: `engine.go:245` (local_shell),
      `beam_cmd.go:326` (shellsession), and `acpsvc/commandrunner.go:35`
      (the non-terminal ACP fallback, which passes no scrub at all).
      `terminalservice` has no production constructor left either.
- [ ] **Confining a mission unit needs a redesign, not wiring** (verdict on
      the old SelfSpawn entry: the carve-out is real and now documented, at
      README §Guardrails and `docs/guide/agent-sandbox.md`). A unit is
      `contenox acp` sharing the control plane BY FILE — it opens
      `~/.contenox/local.db` read-write, seeds `hitl-policy-*.json`, writes
      telemetry logs, and resolves provider credentials from inherited
      `*_KEY` env vars. The wall denies precisely that (`~/.contenox` is
      never carved, secrets are scrubbed), so confining a unit today would
      take a read-WRITE hole over the policy that governs it plus an
      allow-list of the secrets the scrub exists to strip — a wall in name
      only. A real fix moves the unit's control-plane access behind an IPC
      seam (parent-served DB/policy/credentials over the stdio connection it
      already holds) so the unit needs no `~/.contenox`; only then can it be
      walled like a foreign agent.
- [ ] **The wall has no production entrance**: the only spawn it confines is
      an `external_acp` agent, and that kind is not user-registerable — no
      CLI verb (`contenox agent` is list/show/remove/enable/disable),
      no API, and discovery emits chain agents only
      (`runtimetypes/agents.go:25-29`, `chain_agent_discovery.go:34`). So
      `libsandbox` — Landlock, netns, egress guard, syscall tap, ~4k lines
      with real tests — is unreachable on a stock install. Either ship the
      registration path (`agent add --command …`, the piece the deleted
      `/docs/integrations/agents/external-acp-agents/` link promised) or say
      so on the tin; docs now say so (`docs/guide/agent-sandbox.md` status
      note).
- [x] **Budgets half-real** → closed 2026-07-27. `modelAllowlist`/
      `backendAllowlist` are now ENFORCED at the resolution seam
      (`llmrepo/bounds.go`): the host cannot watch a unit's model choice (a
      dispatched unit resolves in its own process and no ACP session update
      carries a model), so the BOUND travels in instead — envelope →
      `fleetservice.Dispatch` → session/new `_meta`
      (`missionservice.MissionMeta`) → `sessionEntry` → turn ctx →
      `llmrepo.WithResolutionBounds`, refusing before anything is sent.
      `onExhausted: "pause_ask"` is NOT implemented (it needs a
      budget-EXTENSION concept the runtime lacks) and now says so where the
      operator looks: `contenox vet` prints a WARN
      (`hitlservice.PolicyDiagnostics`) and the durable stuck reason names the
      substitution. All five presets ship a real `compute` block.
- [ ] **maxTurns is decorative** (found while closing the above): the drive
      loop hard-caps a mission at TWO prompt turns (the intent + one nudge,
      `fleetservice.go` `driveUnattendedMission`), so any `maxTurns` above 1
      can never fire. The bound is real but its useful range is {1}. Either
      make the drive loop's turn count the thing `maxTurns` bounds, or rename
      the field to what it actually gates. The presets omit it on purpose
      today and say why.
- [ ] **`maxToolCalls` counts escalations, not tool calls**: only dispatches
      the unit ESCALATES reach the host answerer, so under an `allow`-heavy
      envelope (`hitl-policy-dev.json`) very little is counted. Honest as
      documented ("envelope-gated tool dispatches"), but it reads like a
      total-actions cap and is not one.
- [ ] **Doc rot from the chop**: 25 code comments cite blueprint files
      deleted in the cull (fleet-consolidation.md, attention-layer.md,
      ide-workflows.md); `operatorinbox` package doc advertises a
      nonexistent `inbox watch` verb; doctor's fleet-counters line is
      unreachable cross-process (process-local atomics,
      `doctor_cmd.go:177`). (The previous §2 claiming beam was unbuilt is
      fixed by this rewrite.)

## 2. Quick wins (cheap, high leverage)

Go:
- [ ] gointel sees `_test.go` (`snapshot.go:145` sets `Tests: false`) —
      `go_references` currently never shows a symbol's tests.
- [ ] go-test filter accepts plain `go test` output (parser requires the
      literal `-json` flag today, `filter_parsers.go:74`).

TS/JS + Python instrumentation (filters + policy only — the intelligence
trap is parked in §6; filters attach to commands the agent already runs in
the sandboxed shell, so they're rule-compliant. Python added 2026-07-27:
the audience reality is Python + React, and the audit had zero Python
coverage beyond the pip-install noise filter):
- [ ] eslint-JSON and vitest/jest output parsers in `nativeOutputParsers`
      (`localtools/filter_parsers.go:57`; each mirrors the golangci-lint
      shape, ~1 file apiece).
- [ ] pytest and ruff/mypy output parsers, same shape (pytest has
      `--json-report` via plugin but plain `-ra` output is parseable;
      ruff has `--output-format json`; mypy has `--output json`).
- [ ] tsc filter upgraded from 3 regexes to structured `--pretty false`
      parsing (`filter_parsers.go:266`).
- [ ] `safeShellPrefixes` entries for npm/npx/node/tsc/eslint AND
      python/pip/uv/pytest/ruff/mypy (`hitlservice/policy.go:951`) —
      every JS and Python command currently falls to approve-tier.

Both languages:
- [ ] Repo-wide grep tool (`local_fs.grep` is single-file,
      `fs_schema.go:70`) + `**` glob support in `find_files`
      (`fs_schema.go:78`).
- [ ] A patch/str-replace edit tool — editing today is full `write_file`
      or `sed`; `unifiedDiff` (`hitl.go:562`) is render-only. Thinnest part
      of the coding story.

Hygiene-adjacent:
- [ ] Extract the triplicated noise/skip-dir matcher
      (`workspaceindex/noise.go:10` calls it overdue; third copy in
      `beamtui/comp/fileaddr/noise.go`). STILL OPEN — and now load-bearing
      for retrieval quality: `_test.go` is 36.8% of the index corpus and
      golden/testdata fixtures outrank real answers, which is a filter
      decision the three copies must agree on.
- [ ] Tests for `libdbexec` (0 tests, 36 importers — most-depended package
      in the repo) and `sessionservice` (0 tests, 3 importers).

## 3. PM-flow slices

- [ ] **Wire missionchanges** — built, 19 tests, zero production callers:
      `contenox mission changes|diff` verb + a beam pane over its
      DOI-ranked changed files, per-path diffs, scope-anomaly flag. Biggest
      PM win available; the code already exists.
- [ ] **The board**: `mission list` status filters/counts; populate
      beamtui's declared-but-never-set `Missions` badge
      (`app/render.go:150` sets only `Inbox`); a reader for
      `presence.Store.List` (write-side only today); a cross-mission
      pending-ask read in hitlservice (kills the N+1 scan behind
      `mission asks`, `mission_cmd.go:719`).
- [ ] **Worktree-per-mission dispatch option** — concurrent missions share
      one cwd with no isolation primitive; mission diffs come from the ACP
      journal, never a git boundary.
- [ ] **Handover consumption** (auto-follow-up from `handoverForNext`,
      today rendered but never machine-consumed) — GATED on D28 (§5).

## 4. goja/TS tool authoring (the in-process track)

- [ ] Embed esbuild (`github.com/evanw/esbuild/pkg/api` — pure Go,
      CGO-free) in gojatool: transpile TS tool scripts at load, cache by
      content hash; bundling turns pure-JS npm packages into tool material.
      Directly answers `sandbox.go:46`'s "bring your own build".
- [ ] Multiple named exports per script file → multiple tools
      (`goja.AssertFunction` per export, schema per export).
- [ ] RULING FIRST (§5): async in the sandbox — pumped promise job queue
      with `host.tool` as the only async source, vs. today's constitutional
      Promise refusal.
- [ ] Whiteroom input: `direktiv-mining.md` (2026-07-27, Apache source read
      directly) — M2: AST-extract the `tool` descriptor + static host.tool
      footprint instead of executing scripts at load (goja ships parser/ast,
      zero new deps); M3: source maps threaded through load AND runtime VM
      so TS errors point at authored lines; M1 resolves the suspension
      question via step-boundary checkpoints (informs the §5 rulings).
      STATUS 2026-07-27: **M2 is next up** and its value grew — declared
      reach (`tools: [...]`, shipped today) is TRUSTED; the static footprint
      makes it VERIFIABLE, so a mismatch becomes a load-time error. **M1 is
      RULED**: adopt step boundaries, but ONLY for detached/mission scripts
      — an attached session is correctly served by the shipped
      stop-the-clock-across-host-calls fix (a human is right there); a
      mission parking for hours on a held goroutine is what needs the
      boundary. Opt-in step API, not a rewrite of the shipped contract; it
      composes with `checkpoints.go`, which already stores name+JSON.

## 5. Open decisions (maintainer)

- [ ] goja async ruling (§4) — touches the sandbox's "no event loop" line.
      Input: `direktiv-mining.md` M4 — the sync-twin convention (ship
      blocking host calls only, defer the event loop) is the cheapest
      answer, and is effectively what shipped; its warning about resolving
      promises from a foreign goroutine against a non-goroutine-safe VM is
      the thing to not copy.
- [ ] sobek-vs-goja evaluation (new, from `direktiv-mining.md`): k6 and
      direktiv both run grafana's maintained fork. Compare ES coverage,
      perf and maintenance cadence against dop251/goja BEFORE any TS slice
      lands, not after.
- [ ] Mission re-entry (blueprint D28): may a finished mission wake its
      supervising agent? pando report §F1-G2/G3 holds the disciplined
      design (budgeted re-entries, await-all/any). Also gates §3 handover
      consumption. If yes → slice it.
- [ ] Orphan cull ruling: `terminalservice`+`terminalstore`, `contextasm`,
      `providerservice`, `accessview`, `agentview` (~3.4k LOC live-looking,
      zero importers, V1-reshape leftovers) — delete or justify each.
- [ ] Harness bench: e2e usability/productivity bench vs opencode (same
      pinned model both sides; Claude Code as flow ceiling). Task suite
      split Go / TS / PM-flow; measure wall-clock-to-green, interventions,
      approval friction, recovery-from-kill, detach-and-return. Split
      verdict expected = roadmap validation, and the detach leg is a
      marketing asset (neither opponent can resume cross-process).
- [ ] Dependabot: 67 findings (8 critical) — likely the website npm tree
      (`cd website && npm audit`)
- [ ] AWS: decommission contenox-modeld-artifacts-573643652148?
- [ ] After next release: install.sh e2e against contenox/beam assets.
      Landing recording (§7) starts from scratch when needed.

## 6. Parked / not-doing (revisit triggers written down)

- **TS type intelligence ("tsintel")** — every current path fails a rule
  or the horse-sense test: typescript.js-inside-goja (native Go compiler
  exists; interpreting the JS checker is the wrong horse), forking
  microsoft/typescript-go (everything under `internal/`, API marked "not
  ready", repo merges into microsoft/TypeScript and closes),
  tsc/tsserver/LSP subprocess tools (violates the no-subprocess-tools
  standing rule). REVISIT TRIGGER: microsoft/typescript-go (or its
  post-merge home) publishes importable Go packages → build tsintel
  in-process, in gointel's image. Context: TS 7.0 GA'd 2026-07-08 as the
  native Go compiler; 7.0 has no public compiler API; 7.1's planned API is
  JS-side.
- **Project-code execution inside goja** — Node-compat treadmill (fs,
  event loop, native addons); k6 precedent says curated host API only.
- **gemini explicit cachedContents** — usage-evidence gate (carried).

## 7. Carried implementation bites

- [ ] Envelope-core follow-ups: the spawned-binary fleet e2e for
      `mission stop` live teardown; a CLI-binary-level e2e for
      `approvals respond` (composed-service path proven by resume_e2e +
      attention_resume_e2e).
- [ ] ACP-editor-side vision sanity check (manual, needs a live editor).
- [ ] Landing: replace the comparison table with a three-frame terminal
      recording (mission fired → detach → envelope interrupt) — beam now
      exists to record it.
- [ ] Onboarding coherence (from the surfaces audit): setup wizard's
      hand-listed 4-provider menu vs 7 supported (self-documented DRIFT
      HAZARD, `setup_cmd.go:50`); fold embed-model setup into `setup` so
      `index`/`search` works first-run; the `chat` vs `acp`/`beam`
      chain+policy divergence and missing session continuity between CLI
      and TUI identities — needs its own slice or a ruling that the split
      is intended.

## 8. Stale-hunt & hygiene

The 2026-07-27 chop ran the full grep list — modeld/make/contenox-runtime/
beam hits fixed or justified, flaky trio + ollama gating fixed, SUPPORT.md
and issue templates verified clean (see ledger). Still standing:
- Re-run the greps before release as a regression check (`modeld|openvino|
  llamacpp`, `mistral|openrouter`, `contenox/runtime`, `beam`, `make `)
- go.mod: beam shipped WITHOUT charmbracelet libs (no-framework architecture,
  `beam-tui.md` §1/§9) — the gate is now no TUI-framework deps at all,
  enforced by testkit's import-boundary gate (`TestUnit_ImportBoundaries`);
  grpc stays indirect-only
- examples/ and .contenox/ chains re-verified against surviving providers;
  maintainer's home dir: `contenox init --update` refreshes the stale
  agent-planner.json that vet caught
