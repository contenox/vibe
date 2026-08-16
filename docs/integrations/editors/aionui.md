---
title: Use Contenox from AionUi
description: Run your contenox chains inside AionUi — a free, local, open-source desktop chat UI for ACP agents.
---

# Use Contenox from AionUi

Prefer a dedicated desktop chat app over an editor panel? [AionUi](https://github.com/iOfficeAI/AionUi) is a free, local, open-source GUI that drives any ACP-compatible agent. Contenox speaks the [Agent Client Protocol](https://github.com/zed-industries/agent-client-protocol) over stdio, so it drops straight in as a custom agent — your chains, tools, and model config, in AionUi's chat UI.

Verified with **AionUi 2.0.0**.

This page assumes you already have `contenox` on `PATH`. If not, do the [Quickstart](/docs/guide/quickstart/) first.

---

## Setup

In AionUi, add a custom agent: **Settings → Agents → add a Custom Agent**, then fill the *Detect Custom Agent* form:

![AionUi — Detect Custom Agent](/aionui-custom-agent.png)

- **Display Name:** `Contenox`
- **Command:** `contenox`
- **Arguments:** `acp`

Hit **Test Connection** — you should see *"Connection successful! CLI exists and ACP protocol is working."* — then **Save**. Or paste the equivalent into **Advanced (JSON)**:

```json
{
  "name": "Contenox",
  "defaultCliPath": "contenox",
  "enabled": true,
  "acpArgs": ["acp"],
  "env": {}
}
```

That's it — pick **Contenox** as the active agent and start a session.

---

## What you get

**Your agent, in a chat UI.** Every prompt runs the ACP session chain, compiled from the `acp` agent declaration — the same agent behavior you'd get from the CLI or any other ACP client, in AionUi's conversation surface.

**Tool steps with real context.** When the chain runs a tool, AionUi's step view shows the actual operation — `local_shell: ls -l`, `local_fs.read_file: README.md` — not just a bare tool name.

**Native filesystem.** `local_fs.read_file` / `local_fs.write_file` / `local_fs.edit_file` route through AionUi's own filesystem capability.

**Approvals in the UI.** When the chain hits a tool in your active [HITL policy](/docs/guide/hitl/), AionUi shows an Allow/Deny dialog instead of a terminal prompt.

**Same everything else.** Models, chains, and MCP servers come from your global contenox config — switch the model with `contenox config set default-model …`, register MCP once with `contenox mcp add`, and AionUi sessions pick it up.

**Missions from the composer.** `/mission <intent>` (or `/mission <agent-name> <intent>`) fires a declared agent at the intent unattended, as a child subprocess of this editor session, and its reports stream live back into the firing session. Configure the fallbacks first (`contenox config set default-mission-agent` / `default-mission-policy`); details in the [Zed guide](/docs/integrations/editors/zed/#fire-missions-with-mission) and the [CLI reference](/docs/reference/contenox-cli/#the-mission-slash-command).

AionUi layers its own chat UI and skill ecosystem on top; the agent itself — declared in a file, with its tools and its policy — is your contenox.

---

## Choosing the chain

ACP sessions load a chain compiled from the `acp` agent declaration (router + coding/general/review loops, under `.contenox/agents/`). Contenox resolves it in order, first match wins: an operator copy at `~/.contenox/<name>.json`, then the compiled `~/.contenox/.generated/<name>.json`, then the shipped `~/.contenox/system/<name>.json`.

Override the path entirely with the `CONTENOX_ACP_CHAIN_PATH` environment variable.

The ACP chain's `"tools": ["*"]` exposes everything the engine has registered — `local_fs`, `local_shell`, plus any MCP servers you've added.

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

**"Connection successful" but every prompt fails with "Agent disconnected".** Update to the latest contenox — older builds rejected AionUi's launch flag on the session path. Current builds accept it; Test Connection and real sessions then behave the same.

**Nothing happens when I select Contenox.** Make sure `contenox` is on AionUi's `PATH`. AionUi inherits the environment of the process that launched it; starting it from a terminal (or using an absolute path in **Command**) is the reliable test.

**The default-model error.** Run `contenox config set default-model <name>` and `contenox config set default-provider <type>` before starting a session.

---

## Limitations

- **No interactive embedded terminal.** AionUi advertises filesystem but not the ACP terminal capability, so `local_shell` commands run and report their output rather than opening a live terminal.

---

## Where to next

- [Declaring agents](/docs/guide/agents/) — one Markdown file is the agent, regardless of which client drives it.
- [Writing a chain by hand](/docs/guide/chains/writing-a-chain/) — for the agent that has outgrown a declaration.
- [HITL policies](/docs/guide/hitl/) — choose what requires approval.
- [MCP](/docs/integrations/tools/mcp/) — register servers once globally; ACP sessions pick them up.
- [Use from Zed](/docs/integrations/editors/zed/) · [Use from JetBrains](/docs/integrations/editors/jetbrains/) — the same agent, other clients.
