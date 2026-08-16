---
title: Use Contenox from Zed
description: Drive your chains from inside the Zed editor over the Agent Client Protocol.
---

# Use Contenox from Zed

Contenox speaks the [Agent Client Protocol](https://github.com/zed-industries/agent-client-protocol) (ACP) over stdio. Zed can launch it as a custom agent server and drive your chain from inside the editor — tool calls render as cards, HITL prompts route through Zed's permission UI, and session history replays when you reopen the project.

This page assumes you already have `contenox` on `PATH`. If not, do the [Quickstart](/docs/guide/quickstart/) first.

---

## Setup

Add Contenox to `~/.config/zed/settings.json`:

```json
{
  "agent_servers": {
    "Contenox": {
      "type": "custom",
      "command": "contenox",
      "args": ["acp"]
    }
  }
}
```

Restart Zed (or reload the window). Open the agent panel — Contenox now appears in the agent picker. Start a new session and prompt as usual.

---

## What you get

**Tool cards with real context.** When the chain runs a shell command, the card shows `local_shell: git status --short` — the actual command, not just the tool name. Same for `local_fs.read_file`, `local_fs.write_file`, `local_fs.sed`, and any other `local_shell` command (`grep`, `rg`, `find`, ...). This is the card you approve from, so it shows what will actually run.

**Native editor surfaces.** `local_fs.read_file`/`local_fs.write_file`/`local_fs.edit_file` route through Zed's own filesystem capability — sandboxed, with a read-before-write contract. `local_shell` runs in a real Zed terminal you can interact with.

**HITL through the editor.** When your chain calls a tool listed in your active HITL policy, Contenox's [HITL policy](/docs/guide/hitl/) applies — and the approval dialog is routed to Zed's permission UI instead of a terminal prompt. The default policy gates `local_fs.write_file`, `local_fs.edit_file`, `local_fs.sed`, and `local_shell.*` calls.

**Session history that replays.** Close Zed mid-conversation and reopen the project — your prompts, the agent's responses, and every tool call (with its output) come back. State lives in `~/.contenox/local.db`.

---

## Choosing the chain

ACP sessions load a chain compiled from the `acp` agent declaration (router + coding/general/review loops, under `.contenox/agents/`). Contenox resolves it in order, first match wins: an operator copy at `~/.contenox/<name>.json`, then the compiled `~/.contenox/.generated/<name>.json`, then the shipped `~/.contenox/system/<name>.json`.

Override the path entirely with the `CONTENOX_ACP_CHAIN_PATH` environment variable (set it in the shell that launches Zed).

The ACP chain looks like any other Contenox chain. Its `"tools": ["*"]` exposes everything the engine has registered — `local_fs`, `local_shell`, plus any MCP servers you've added via `contenox mcp add`.

---

## Choosing the model

ACP reads from your global model/provider config — the same one the CLI uses:

```bash
contenox config set default-model qwen3:8b
contenox config set default-provider ollama
```

Models are global config, shared across every surface that reads `default-model` — switching it here switches it everywhere.

---

## HITL approval flow

When the chain calls a tool listed in your active HITL policy (default: `local_fs.write_file`, `local_fs.edit_file`, `local_fs.sed`, `local_shell.*`), Contenox emits an ACP permission request which Zed renders as an approval dialog. The card shows the actual command/path, so you approve the specific operation — not a bare tool name.

To skip Contenox HITL entirely (trusted/scripted contexts), launch with `--auto`:

```json
{
  "agent_servers": {
    "Contenox": {
      "type": "custom",
      "command": "contenox",
      "args": ["acp", "--auto"]
    }
  }
}
```

`--auto` disables Contenox HITL — every gated tool runs without prompting. Use it deliberately.

---

## Fire missions with `/mission`

Type `/mission <intent>` (or `/mission <agent-name> <intent>`) in the agent panel to fire a [mission](/docs/reference/contenox-cli/#the-mission-slash-command) without leaving the conversation: a declared agent runs the intent unattended under its envelope, as a child subprocess of this editor session. The unit's reports stream live back into the session that fired it.

> **Beta:** naming an agent of your own — a declaration in `.contenox/agents/`, or a hand-authored `chain-agent-*` chain — requires `contenox config set opt-in-beta true` (or `CONTENOX_OPT_IN_BETA=1`); `/mission` itself and the shipped `agent-planner` work without it.

Set the fallbacks the bare form uses first:

```bash
contenox config set default-mission-agent  <agent-name>
contenox config set default-mission-policy <hitl-policy-file>
```


---

## Troubleshooting

**Nothing happens when I select Contenox.** Make sure `contenox` is on Zed's `PATH`. Zed inherits the shell environment of the GUI process — on Linux that's usually your login shell's `PATH`. Test with `which contenox` in a shell launched from the same desktop session.

**The default-model error.** ACP needs a configured default model. Run `contenox config set default-model <name>` and `contenox config set default-provider <type>` before launching from Zed.

**I want to see what's happening.** Enable file logging:

```bash
contenox config set telemetry-enabled true
```

Subsequent ACP sessions write structured operation traces to `~/.contenox/telemetry.log` (chain steps, tool calls, model requests, session updates sent to Zed). Stderr from the agent process also lands in Zed's `Zed.log`.

---

## Where to next

- [Declaring agents](/docs/guide/agents/) — one Markdown file is the agent, regardless of which client drives it.
- [Writing a chain by hand](/docs/guide/first-chain/) — for the agent that has outgrown a declaration.
- [HITL policies](/docs/guide/hitl/) — choose what requires approval and what doesn't.
- [MCP](/docs/integrations/tools/mcp/) — register MCP servers once globally; ACP sessions pick them up automatically.
