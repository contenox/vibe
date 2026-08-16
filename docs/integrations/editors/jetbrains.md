---
title: Use Contenox from JetBrains
description: Drive your chains from inside GoLand, IntelliJ IDEA, and other JetBrains IDEs over the Agent Client Protocol.
---

# Use Contenox from JetBrains

Contenox speaks the [Agent Client Protocol](https://github.com/zed-industries/agent-client-protocol) (ACP) over stdio. JetBrains IDEs can launch it as a custom agent server and drive your chain from inside the editor — tool calls render as cards, HITL prompts route through the IDE's permission UI, and session history replays when you reopen the project.

Verified with **GoLand 2026.1.2**. The config file is JetBrains-wide, so the same setup applies to other JetBrains IDEs (IntelliJ IDEA, PyCharm, WebStorm, …) with the ACP agent integration.

This page assumes you already have `contenox` on `PATH`. If not, do the [Quickstart](/docs/guide/quickstart/) first.

---

## Setup

JetBrains reads agent servers from `~/.jetbrains/acp.json`. Note this is **not** the Zed schema — there is no `"type": "custom"` field:

```json
{
  "default_mcp_settings": {
    "use_custom_mcp": true,
    "use_idea_mcp": false
  },
  "agent_servers": {
    "Contenox": {
      "command": "contenox",
      "args": ["acp"]
    }
  }
}
```

Restart the IDE. Open the agent panel — Contenox now appears in the agent picker. Start a new session and prompt as usual.

---

## What you get

**Tool cards with real context.** When the chain runs a shell command, the card shows `local_shell.local_shell: sed -i 's/old/new/g' README.md` — the actual command, not just the tool name. Same for `local_fs.read_file`, `local_fs.write_file`, `local_fs.sed`, and any other `local_shell` command (`grep`, `rg`, `find`, ...). This is the card you approve from, so it shows what will actually run.

**Native filesystem.** `local_fs.read_file` / `local_fs.write_file` / `local_fs.edit_file` route through the IDE's own filesystem capability — sandboxed, with a read-before-write contract.

**Shell commands.** `local_shell` runs the command and returns its output in the tool card. GoLand does not advertise the ACP terminal capability, so the command is executed and reported rather than embedded as an interactive terminal session (this differs from [Zed](/docs/integrations/editors/zed/), which does embed a live terminal).

**HITL through the editor.** If your chain uses `local_fs`/`local_shell`, Contenox's [HITL policy](/docs/guide/hitl/) applies — and the approval dialog is routed to the IDE's permission UI instead of a terminal prompt. The default policy gates `local_fs.write_file`, `local_fs.edit_file`, `local_fs.sed`, and `local_shell.*` calls.

**Session history that replays.** Close the IDE mid-conversation and reopen the project — your prompts, the agent's responses, and every tool call (with its output) come back. State lives in `~/.contenox/local.db`.

**Missions from the composer.** `/mission <intent>` (or `/mission <agent-name> <intent>`) fires a declared agent at the intent unattended, as a child subprocess of this editor session, and its reports stream live back into the firing session. Configure the fallbacks first (`contenox config set default-mission-agent` / `default-mission-policy`); details in the [Zed guide](/docs/integrations/editors/zed/#fire-missions-with-mission) and the [CLI reference](/docs/reference/contenox-cli/#the-mission-slash-command).

---

## Choosing the chain

ACP sessions load a chain compiled from the `acp` agent declaration (router + coding/general/review loops, under `.contenox/agents/`). Contenox resolves it in order, first match wins: an operator copy at `~/.contenox/<name>.json`, then the compiled `~/.contenox/.generated/<name>.json`, then the shipped `~/.contenox/system/<name>.json`.

Override the path entirely with the `CONTENOX_ACP_CHAIN_PATH` environment variable (set it in the shell that launches the IDE).

The ACP chain's `"tools": ["*"]` exposes everything the engine has registered — `local_fs`, `local_shell`, plus any MCP servers you've added via `contenox mcp add`.

---

## Choosing the model

ACP reads from your global model/provider config — the same one the CLI uses:

```bash
contenox config set default-model qwen3:8b
contenox config set default-provider ollama
```

Models are global config, shared across every surface that reads `default-model` — switching it here switches it everywhere.

---

## Troubleshooting

**Nothing happens when I select Contenox.** Make sure `contenox` is on the IDE's `PATH`. JetBrains inherits the environment of the process that launched it — if you start the IDE from a desktop launcher, that may not be your login shell's `PATH`. Launching the IDE from a terminal (or using an absolute path in `command`) is the reliable test.

**The default-model error.** ACP needs a configured default model. Run `contenox config set default-model <name>` and `contenox config set default-provider <type>` before launching from the IDE.

**I want to see what's happening.** Enable file logging:

```bash
contenox config set telemetry-enabled true
```

Subsequent ACP sessions write structured operation traces to `~/.contenox/telemetry.log` (chain steps, tool calls, model requests, session updates sent to the IDE).

---

## Limitations

- **No interactive embedded terminal.** GoLand does not advertise the ACP terminal capability; `local_shell` commands run and report output rather than opening a live terminal in the IDE.

---

## Where to next

- [Declaring agents](/docs/guide/agents/) — one Markdown file is the agent, regardless of which client drives it.
- [Writing a chain by hand](/docs/guide/chains/writing-a-chain/) — for the agent that has outgrown a declaration.
- [HITL policies](/docs/guide/hitl/) — choose what requires approval and what doesn't.
- [MCP](/docs/integrations/tools/mcp/) — register MCP servers once globally; ACP sessions pick them up automatically.
- [Use from Zed](/docs/integrations/editors/zed/) — the same agent, driven from Zed (with a live embedded terminal).
