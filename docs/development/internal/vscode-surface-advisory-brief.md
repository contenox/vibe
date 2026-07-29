# Advisory brief: what the VS Code extension should become — 2026-07-28

**This document is a prompt.** It is written for an external advisor model, not
for the maintainer. If you are that model: read this whole file, read the
repository paths it cites, then write your advice to the landing path in §8.
Do not modify any other file.

---

## 1. The question

The VS Code extension was deleted on 2026-07-26 (`7ffadb05`) and restored on
2026-07-28 into the current repository layout. It works: it builds, packages,
and its integration tests pass. It is also, feature-wise, exactly what it was
in July — a chat panel, autocomplete, a few code actions, and a settings view.

The maintainer's intent is larger, and is quoted here in full because your
advice has to serve it rather than a paraphrase of it:

> it's unclear yet if the TUI stays and if the TUI will be the primary surface;
> the vscode extension should be everything you expect out of an IDE assistant
> with everything it already could do and all the learning from the TUI and all
> the learnings from the old contenox serve. The core reasoning is that it
> likely is less of a maintenance commitment than the old contenox serve while
> still giving a UI for non-CLI devs (which are many), especially since ACP,
> while a nice protocol, turned out a closed distribution door.

**Answer this:** what should the VS Code extension *be*, how should it be built
against the existing Go core, in what order should it get there, and what does
that imply for the TUI. Take positions. A survey of options with no
recommendation is a failed brief.

---

## 2. Ground truth: the repository as it stands

Verify anything below before you rely on it; paths are given so you can. Where
this brief and the code disagree, the code wins — say so in your advice.

**Shape.** One Go module, `github.com/contenox/contenox`. Repo `contenox/beam`
on the remote, being renamed to `contenox/contenox` (WORK.md item 12). Product
is `contenox`. The TUI has **no name of its own** — it is invoked as
`contenox new [dir]` and presents itself as "contenox" (renamed from `contenox
beam` on 2026-07-28). "Beam" now refers only to the retired React web UI in the
Lab. The Go package is still `internal/surfaces/beamtui` and the preset files
are still `chain-beam.json` / `hitl-policy-beam.json`: internal names were left
alone deliberately, so do not read them as a surviving product name. Task
(`Taskfile.yml`) is the build system.

**Surfaces** live under `internal/surfaces/`:

| Surface | What it is |
|---|---|
| `contenoxcli` | the CLI; also the composition root that builds the engine (`engine.go`, `BuildEngine`) |
| `beamtui` | the terminal UI (`contenox new [dir]`) |
| `acpsvc` | Agent Client Protocol server (`contenox acp`, `contenox acpx`) over `libacp` |
| `vscodeagent` | the VS Code bridge (`contenox vscode-agent --stdio`) — restored 2026-07-28 |
| `fleetboot` | fleet/presence bootstrap |

Everything below the surfaces is shared: `internal/kernel/{taskengine,enginesvc,
reasoning,nativeturn,contextasm,tools,llmresolver,agentinstance}`,
`internal/services/*` (~40 packages: missions, HITL, approvals, sessions,
agents, gointel, localtools, MCP, sandbox, presence, inbox…),
`internal/models/*`, `internal/store/*`.

**What the extension does today** (`packages/vscode`): 38 commands, 3 views
(chat webview, sessions tree, runtime-controls webview), 15 settings, a real
VS Code **language-model chat provider** registration (vendor `contenox`), an
**MCP server definition provider**, one **language-model tool**
(`approve_contenox_tool_call`) used to raise native approval UI, inline
completion via a separate FIM model, editor code actions (ask/fix selection,
fix/explain diagnostics, review changes, draft commit message), a 5-step
walkthrough. No proposed APIs are enabled. `extensionKind: ["workspace"]`, and
it bundles a per-target `contenox` binary in `bin/`, so it works over SSH, WSL,
containers and Codespaces.

**What the bridge exposes** (`internal/surfaces/vscodeagent/server.go`) — the
complete method list, 18 of them:

```
initialize  health  shutdown
chatSend  chatCancel
sessionCreate  sessionList  sessionLoad  sessionRead  sessionDelete
getConfig  setConfig  listCommands  listModels  listProviders
listHitlPolicies  listMCPServers  autocomplete
```

Plus in-chat slash commands (`commands.go`): `help doctor clear compact model
provider autocomplete-model autocomplete-provider max-tokens think policy
capability websearch`.

**What ACP already covers that the bridge does not.** `libacp` implements ACP v1
for both roles; `acpsvc` serves: `session/{new,load,list,delete,close,cancel,
prompt,resume,request_permission,set_config_option,set_mode,set_model,update}`,
`fs/{read,write}_text_file`, `terminal/{create,kill,output,release,
wait_for_exit}`, plus mission dispatch, plan projection, image content, and
external-agent adoption (`external.go`, `adopt.go`). The bridge has no terminal,
no fs delegation, no modes, no plan, no missions, no resume.

**What the TUI has** (`internal/surfaces/beamtui`): components for approval,
composer, file-address completion, command palette, picker, statusbar,
transcript; an `enginebridge` with a live **inbox watch**; slash commands
`/compact /help /mission /model /new /quit /rename /sessions /src`; a keymap
layer, liveness, ANSI/style tiering, and a `testkit` with symbolic style tags so
goldens survive recolors.

**What the old web UI had, and where it went.** `contenox serve` and its React
SPA were deleted in the same purge. The admin surface was, by page:
`backends chains control fleet hitl-policies inbox missions models prompt
remotehooks settings` plus chat. The `serve` command no longer exists; the
retrospective is `docs/rnd/beam-web.md`. The React workspace (`packages/beam`,
`packages/ui`, 577 files) is **gone and is not coming back** — see §5. When the
extension was restored, the 27 UI files its webview actually needed were
vendored into `packages/vscode/webview-src/ui/` rather than resurrecting that
workspace.

**The capability inventory is the CLI.** Anything the product can do, it can do
from `contenox`: `agent approvals backend cache chat config doctor inbox index
init mcp mission model run sandbox search session setup shell-env state tools
update vet workspace`. Read that list as the menu an IDE assistant could expose.

**The shared spine.** Two contracts constrain any new surface, and both are
CI-asserted:

- `docs/development/engine-events.md` — normative, additive-only. Every consumer
  of `taskengine.TaskEvent` (the ACP translators, the TUI engine-bridge, the
  bridge's `bridgeEventSink`, the journal) is bound by the per-kind matrix.
- HITL: policy and tool identity live in the runtime, never in a client.
  Approvals route through the engine and are rendered by the surface. Policies:
  `hitl-policy-{default,strict,dev,acp,acpx,beam}.json`.

**Strategy already settled** (`docs/development/internal/2026-07-28-session-memo.md`,
`WORK.md`) — do not re-litigate these, build on them:

- Platform-dependence is never load-bearing again. Ollama shipped its own agent
  TUI; **VS Code gated its agent surface**; the ACP registry merged zero
  independent agents across its last 100 PRs (PR #353 parked). Ecosystems are
  optional reach; the binary plus un-gated channels is the foundation.
- Positioning is bottom-up, felt-pain language: "an agent you can walk away
  from." The word "governance" is banned from user-facing copy.
- Adoption rewards familiarity, not novelty. Demo a boring thing working on a
  real outcome.
- The maintainer is solo. Attention is the scarce resource; every subsystem is a
  permanent tax.

---

## 3. The tension you are being asked to resolve

Note the collision, because it is the actual problem: the settled strategy says
**never make a gated platform load-bearing**, and VS Code is a gated platform
that has already gated its agent surface. The maintainer's reasoning is that the
extension is still cheaper than `serve` and still reaches non-CLI developers.
Both things are true. Your job is to say what follows from that — not to pick
one and ignore the other.

---

## 4. Decisions you must take a position on

1. **Bridge architecture.** Keep `vscodeagent`'s bespoke JSON-RPC bridge, or
   converge the extension onto the ACP/native-turn machinery in `acpsvc`? Today
   there are three translators of the same event stream (acpsvc's
   `publishEvent`, its `nativeEventTranslator`, and the bridge's
   `bridgeEventSink`). Is that one too many, or is the bespoke bridge earning
   its keep by not being constrained by ACP's wire shape?
2. **What "everything you expect from an IDE assistant" means in 2026** — as a
   concrete feature list, split into table stakes (absence is disqualifying),
   differentiators (contenox's envelope/HITL/mission model), and traps (things
   that look mandatory and are not worth a solo maintainer's attention).
3. **Which of the eleven dead admin pages belong in an IDE at all**, page by
   page. Some are operator surfaces that were never an IDE's job. Say which, and
   where the rest should live if not in the editor.
4. **What the TUI becomes.** If the extension is primary: is `beamtui` retired,
   frozen, or kept as the un-gated channel that the strategy says must exist?
   What is the honest cost of keeping both?
5. **Distribution, given the closed door.** Marketplace only, or Open VSX for
   VSCodium and the forks? What does "un-gated" mean for an extension, and what
   is the fallback if Microsoft gates the APIs this depends on?
6. **Sequencing under a solo budget.** Ordered, with a rough cost per step and an
   explicit statement of what is *not* being built.

---

## 5. Hard boundaries

These are settled. Advice that violates one is unusable.

- **No resurrection of the React workspace.** `packages/beam` and `packages/ui`
  stay deleted. The webview's vendored copy in `webview-src/ui/` is the pattern:
  bring in the closure you need, own it, no sibling package.
- **No return of `contenox serve`**, no HTTP API layer, no `apiframework`. That
  is the maintenance commitment the whole pivot exists to avoid.
- **One Go module.** New surface code goes under `internal/surfaces/`. No new
  repos, no submodules, no second language runtime beyond the extension's own
  TypeScript.
- **The extension stays a workspace extension** bundling a per-target binary.
  Remote SSH / containers / Codespaces support is not negotiable; anything that
  assumes the extension runs on the user's laptop is wrong.
- **Marketplace identity is fixed**: publisher `contenox`, extension
  `contenox-runtime`, id `contenox.contenox-runtime`.
- **Policy, tool identity and approval semantics stay in the runtime.** The
  editor renders decisions; it never owns them.
- **The engine event contract is additive-only** and CI-asserted. A new surface
  subscribes and tolerates unknown kinds; it does not reshape the stream.
- **Do not propose enabling VS Code proposed APIs for the stable package.** A
  proposed-API dependency cannot ship to the Marketplace.
- **No telemetry that leaves the machine.** Local JSONL only, as today.
- **Solo maintainer.** Anything whose steady-state cost needs a second person is
  out, however good it is. Say so when you reject something for this reason.

---

## 6. Where you may push back

Everything in §4 is genuinely open. So is this: if you think the premise is
wrong — that the extension should *not* be the primary surface, or that the TUI
should be, or that neither should and the answer is something else entirely —
say that first and argue it. The maintainer has changed direction on stronger
evidence than a brief. What is *not* open is §5.

---

## 7. Rules of engagement

- **Read-only.** Do not edit, refactor, or "fix" anything. Write exactly one
  file, at the path in §8.
- **Cite paths.** Claims about this codebase carry a file path. If you did not
  read it, do not assert it.
- **Say "I don't know."** An honest gap is worth more than a confident
  fabrication; this repo has been burned by plausible-sounding drift before.
- **No flattery, no recap of this brief.** Start with the recommendation.
- **Prose, not bullets-only.** House style is short paragraphs that commit to a
  claim; see `docs/development/internal/ee-buy-vs-build.md` and
  `inference-stack-decision.md` for the register. Code comments in this repo are
  terse; documents are not.
- **Cost everything in maintainer-attention**, not in story points.
- **Where 2026 external facts matter** (VS Code API gating, Open VSX reach,
  competing IDE assistants), state what you actually know and flag what needs
  checking. Do not present a training-cutoff guess as current.

---

## 8. Where to land the advice

Write a single new file:

```
docs/development/internal/vscode-surface-advice-<advisor>.md
```

Replace `<advisor>` with your own short model name, lowercase, e.g.
`vscode-surface-advice-opus.md`. Do not overwrite this brief. Do not touch
`WORK.md` — the maintainer folds accepted advice into it.

Required structure:

1. **Recommendation** — the call, in under 150 words, first thing on the page.
2. **What the extension should be** — the feature set, split table stakes /
   differentiators / rejected, with the rejection reason on each rejected item.
3. **Architecture** — bespoke bridge vs. ACP convergence, decided, with the
   consequence for the three event translators.
4. **The eleven admin pages** — a table: page, verdict (IDE / CLI-only / dead),
   one-line reason.
5. **The TUI** — retire, freeze, or keep, and what that costs.
6. **Distribution** — Marketplace, Open VSX, and the fallback if the APIs gate.
7. **Sequence** — ordered steps, rough attention cost each, and an explicit
   "not doing" list.
8. **What would change my mind** — the evidence that would invert your call.
9. **Unknowns** — what you could not verify, and how the maintainer could.

No front matter — this folder is unpublished and its files open on an `#`
heading. Title it `# VS Code surface advice (<advisor>) — <date>`.

One more constraint on the file itself: this repo is public even though this
folder is unpublished. No credentials, no account identifiers, no
reproduction-grade security detail.
