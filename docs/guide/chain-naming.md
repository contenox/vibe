---
title: "Chain files: naming, roles, and resolution"
description: The chain-<role>-<variant>.json convention, what each role means, where each chain is selectable, and exactly what contenox init touches.
---

# Chain files: naming, roles, and resolution

Every chain file Contenox ships follows one grammar:

```
chain-<role>-<variant>.json
```

The **role** says what kind of work the chain does and therefore *where it is selectable*. The **variant** distinguishes chains within a role — `default`, `conservative`, or any name you choose for your own. Roles communicate; nothing gates: a valid chain file is selectable in every context its role appears in. There is no registry to update and no approval step — the filename is the declaration.

One name everywhere is the rule behind the grammar: the name embedded in the binary, the name `contenox init` seeds to disk, and the name the docs use are the same string. When a page says `chain-agent-acp.json`, that is the literal file in `~/.contenox/`.

## The roles

| Role | What it does | Where it is selectable |
| ---- | ------------ | ---------------------- |
| `agent` | A conversational, tool-using loop | Session and editor surfaces (`--chain`, the `default-chain` config, per-surface env overrides) and mission dispatch: chain-agent discovery declares every `chain-agent-*.json` file as a fleet-dispatchable agent |
| `planner` | Holds and evolves a mission's living plan | Mission planning: the default agent behind `/mission` and `contenox mission fire` |
| `oracle` | Reviews routine mission questions in unattended runs | Mounted in-process by `contenox mission fire --oracle` — no trigger, no dispatcher, no second process; see [The attention oracle](/docs/use-cases/auto-attention/) |
| `compact` | Summarizes conversation history | The compaction machinery: `/compact` in editor and terminal-UI sessions, `contenox session fork --summary` |
| `fim` | Fill-in-the-middle completion | Editor autocomplete (`_contenox/autocomplete`) |

What every `agent` chain shares — the bounded ReAct loop inside it — is mapped in [The agentic loop](/docs/guide/agentic-loop/).

The seeded set:

- `chain-agent-contenox.json` — interactive CLI chat: `contenox chat` and a bare `contenox "..."`
- `chain-agent-run.json` — the one-shot, stateless `contenox run` loop
- `chain-agent-acp.json` — editor (ACP) sessions
- `chain-agent-acpx.json` — the headless / untrusted-driver ACP profile
- `chain-agent-beam.json` — attended terminal sessions
- `chain-planner-default.json` — the default mission planner
- `chain-compact-default.json` — history compaction
- `chain-fim-default.json` — editor autocomplete
- `chain-oracle-default.json` / `chain-oracle-conservative.json` — the [auto-attention](/docs/use-cases/auto-attention/) oracle variants; seeded like everything else but inert until `contenox mission fire --oracle` mounts one (a beta-only flag)

`trigger-*.json` and `hitl-policy-*.json` are different kinds of files (event triggers and HITL envelopes) and keep their own conventions; init seeds no trigger files (triggers are operator-authored) and seeds `hitl-policy-oracle.json` — the [attention oracle's](/docs/use-cases/auto-attention/) envelope — alongside the other policy presets.

> **Note:** the fleet agent's *name* is the chain's `id` field, not its filename. The seeded planner's id is `agent-planner`, so `contenox mission fire agent-planner` and a stored `default-mission-agent` config are stable however the file is named.

### Declaring your own agent

Name the file `chain-agent-<something>.json` and put it in the workspace
`.contenox/` (or `~/.contenox/`). Discovery runs when a host starts — a
`mission fire`, an editor session — and reconciles the registry from disk.

The **filename** makes the chain eligible. The **`id`** becomes the agent name
you fire at, so these two do not have to match and usually will not:

```jsonc
// .contenox/chain-agent-vaultfiler.json
{ "id": "chain-vaultfiler", ... }
```

```bash
contenox agent list                       # NAME is chain-vaultfiler
contenox mission fire chain-vaultfiler "…" --policy <envelope> --wait
```

Rename the file freely; the agent keeps its name. Change the `id` and you have
renamed the agent, and anything referencing the old name — a stored
`default-mission-agent`, a trigger — stops resolving. A full worked example is
in [Tutorial: a mission agent](/docs/guide/tutorial-mission-agent/).

## Resolution: which file wins

For files resolved by name — the CLI chat chain, the run chain, the compact chain, trigger-referenced chains and policies — resolution is workspace-first:

1. the workspace `.contenox/<name>` (found by walking up from your current directory, like `.git/`), then
2. `~/.contenox/<name>` as the fallback.

A workspace file wins by name. That is the whole override mechanism: put a same-named file in the workspace `.contenox/` and it shadows the global copy. `contenox doctor` lists every shadowing copy it finds.

The ACP surfaces resolve differently, by design: `contenox acp` and `acpx` load their chain from `~/.contenox/` only, each overridable with its own environment variable — `CONTENOX_ACP_CHAIN_PATH`, `CONTENOX_ACPX_CHAIN_PATH`, and `CONTENOX_ACP_FIM_CHAIN_PATH` for autocomplete. An editor may be launched from anywhere, so these surfaces anchor to the home directory rather than a cwd walk; the env var is the per-launch override.

## What `contenox init` touches — and never touches

| Invocation | Touches | Never touches |
| ---------- | ------- | ------------- |
| `contenox init` | Writes the `.contenox/workspace.id` marker (an existing marker keeps its id); seeds the chain files and `hitl-policy-*.json` presets into `~/.contenox/` **only where absent**; prints a note for every workspace copy that shadows a global file | Existing files (it never overwrites), workspace chain files, config, sessions, the database |
| `contenox init --local` | Same seeding, into the **workspace** `.contenox/` instead — deliberate overrides that shadow the global copies by name | `~/.contenox/` chain files |
| `contenox init --force` | Overwrites every seeded chain file and rewrites the HITL presets across the whole search path (home and any shadowing workspace copy) — your edits to seeded files are replaced | User-authored files (anything init does not seed), config, sessions |
| `contenox init --update` | First **renames** any shipped chain file still carrying a pre-v0.38 name to its new name (see below); then refreshes a seeded file only when its checksum matches a known unmodified prior build — edited files are skipped and reported | Hand-edited content (a rename moves bytes, the refresh skips them), user-authored files, config, sessions |
| `contenox init --refresh-policies` | Rewrites **only** the `hitl-policy-*.json` presets from this build, in `~/.contenox` and any workspace copy that shadows one — this is what `contenox doctor` points at when an envelope predates a shipped toolset | Chain files, config, sessions |

## Migrating an old install

Before v0.38 the seeded files carried per-surface names (`default-chain.json`, `default-acp-chain.json`, `headless-acp-chain.json`, `default-beam-chain.json`, `default-run-chain.json`, `default-fim-chain.json`, `chain-compact.json`, `agent-planner.json`). Resolution now looks up only the new names. `contenox init --update` performs the migration:

- Each shipped legacy-named file is **renamed** to its new name, byte-for-byte — a rename, never a rewrite, so a hand-edited chain keeps your content under its new name.
- The rename runs in **both** `~/.contenox/` and the workspace `.contenox/`: a workspace override left under its legacy name would silently stop shadowing.
- If a legacy name and its new name both exist, the new file wins untouched; the legacy file is left in place with a one-line note, never deleted.
- The step is idempotent — a second `--update` finds nothing to rename.

For example, `chain-agent-acp.json` (formerly `default-acp-chain.json`) is the editor chain after one `contenox init --update`.

Your **own** files are yours: `--update` never renames or rewrites anything init did not seed. One consequence of the clean cut is discovery — fleet chain-agent discovery keys on the `chain-agent-*` filename, so a custom agent chain you named `agent-mybot.json` under the old convention is no longer discovered. Renaming it to `chain-agent-mybot.json` is the whole migration; its agent name (the chain `id`) does not change.

## Next

- [Your first chain](/docs/guide/first-chain/) — author a chain from scratch
- [Core concepts](/docs/guide/concepts/) — chains, tasks, tools, transitions
- [`contenox init` reference](/docs/reference/contenox-cli/#contenox-init-provider) — every flag
