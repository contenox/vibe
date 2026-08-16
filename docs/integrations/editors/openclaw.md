---
title: Use Contenox from OpenClaw
description: Run Contenox as a hardened, contained ACP agent driven by OpenClaw over a chat channel — the untrusted-driver profile.
---

# Use Contenox from OpenClaw

This guide covers one specific integration: driving Contenox from OpenClaw through the **`contenox acpx`** profile. It is deliberately the *untrusted-driver* path — other ways to integrate Contenox with OpenClaw may follow; this page is not about those.

[OpenClaw](https://docs.openclaw.ai/) is a personal/team assistant reached from chat channels (Telegram, Slack, Discord, …). It can route a conversation to an external agent over the [Agent Client Protocol](https://github.com/zed-industries/agent-client-protocol) through its `@openclaw/acpx` backend. Contenox speaks ACP over stdio, so `contenox acpx` plugs in directly — as a *contained* agent governed by an authored policy, not a coding tool. This integration exists to demonstrate exactly that containment: an untrusted chat channel driving a Contenox agent that can only do what its policy permits.

Why `acpx` and not `acp`: prompts arrive from a chat inbox with no human at an editor to approve actions. So `contenox acpx` ships a **static containment policy** instead of interactive approval — the boundary the [nested-permission-bomb](/docs/use-cases/nested-permission-bomb/) argues for. You don't hand an untrusted channel your keyring; you give it an authored, version-controlled allow/deny boundary.

Assumes `contenox` is installed and configured with a default model (`contenox setup`, or a configured backend plus `contenox config set default-model …`). Verified against **OpenClaw 2026.5.12**.

> **Use a chat channel, not the dashboard.** OpenClaw's Control UI webchat cannot bind to an ACP agent. Drive Contenox from a bindable channel — Telegram (used here), Discord, Slack, or iMessage.

---

## Setup

**1. Install OpenClaw and start the gateway:**

```bash
curl -fsSL https://openclaw.ai/install.sh | bash      # Windows: iwr -useb https://openclaw.ai/install.ps1 | iex
openclaw config set gateway.mode local
openclaw gateway install
systemctl --user restart openclaw-gateway.service
openclaw dashboard --no-open                           # tokenized dashboard URL
```

The setup wizard's model picker configures OpenClaw's *own* assistant, not Contenox — skip it.

**2. Install the acpx backend:**

```bash
openclaw plugins install @openclaw/acpx
openclaw config set plugins.entries.acpx.enabled true
systemctl --user restart openclaw-gateway.service
```

**3. Register Contenox as an acpx agent** in `~/.openclaw/openclaw.json`:

```json
{
  "plugins": { "entries": { "acpx": { "enabled": true, "config": {
    "agents": { "contenox": { "command": "/absolute/path/to/contenox", "args": ["acpx"] } },
    "nonInteractivePermissions": "deny"
  } } } },
  "acp": { "allowedAgents": ["contenox"], "defaultAgent": "contenox" }
}
```

`command` must be the **absolute path** (`command -v contenox`) — the gateway runs as a systemd user service and does not inherit your shell `PATH`. `nonInteractivePermissions: "deny"` makes OpenClaw honour Contenox's boundary; never set `permissionMode: approve-all`. Restart the gateway, then in the OpenClaw chat run `/acp doctor` — expect `agent=contenox`, `command=… acpx`, `healthy: yes`.

---

## The OpenClaw agent workspace

OpenClaw runs each agent in a working directory, by default `~/.openclaw/workspace`, and seeds it with convention files:

```
AGENTS.md  BOOTSTRAP.md  HEARTBEAT.md  IDENTITY.md  SOUL.md  TOOLS.md  USER.md  state/
```

- **IDENTITY.md / SOUL.md** — the agent's persona and behavioural framing.
- **AGENTS.md** — agent roster and standing instructions.
- **TOOLS.md** — the shell commands/binaries OpenClaw tells the agent it may use.
- **USER.md** — operator/context notes.
- **BOOTSTRAP.md / HEARTBEAT.md** — startup and keep-alive lifecycle prompts.
- **state/** — OpenClaw's runtime state.

This directory matters for Contenox: it is the **working directory `contenox acpx` runs in** (`/acp doctor` shows `cwd=…/.openclaw/workspace`). So the read-only window the hardened policy permits is rooted *here* — the agent sees these OpenClaw convention files, not your project tree, unless you point it elsewhere with an explicit `cwd`. `TOOLS.md` in particular declares shell tooling for OpenClaw's *own* agents; under `acpx` that shell surface is denied (see below) — it does not transfer to Contenox.

---

## Bind a channel (Telegram)

Pointing an untrusted chat inbox at a tool-capable agent is a deliberate decision. It is safe here only because the hardened policy contains it.

```bash
openclaw channels add --channel telegram --token '<your-bot-token>'
systemctl --user restart openclaw-gateway.service
```

Then, as the operator:

1. DM the bot once; it returns a pairing code. Approve it: `openclaw pairing approve telegram <code>`.
2. Make yourself command owner (required for `/acp spawn`): `openclaw config set commands.ownerAllowFrom '["telegram:<your-id>"]'`, then restart the gateway. (`@userinfobot` returns your id.)
3. In the DM: `/acp spawn contenox --bind here`. That conversation now drives Contenox.

---

## What `contenox acpx` can and cannot do

`contenox acpx` runs under `hitl-policy-acpx.json` — a **pure allow/deny** policy, no approval tier, because an untrusted non-interactive channel has no one to approve anything.

**Works:**

- The full agentic loop — Contenox answers, reasons, and uses its read tools, driven from the bound chat.
- Reading a file whose path you already know: `read_file`, `read_file_range`.

**Does not work — denied by design:**

- Shell / command execution (`local_shell`) — entirely, so there is also no directory listing, search, or glob (those live in the shell now, not in `local_fs`).
- Any file write or edit (`write_file`, `edit_file`, `sed`).
- Any network call — there is no built-in network tool; the only way this profile could reach the network is an MCP server the operator explicitly attaches, and none is attached by default.
- Reads of credential, key, dotfile, and secret paths.
- Anything not explicitly allowed — `default_action: deny`.
- Interactive approval — there is no "ask the operator" tier on this profile; an action is allowed or it is refused.

This is stricter than the device-owner profile, not weaker. Interactive HITL belongs to the `acp` profile (Zed/JetBrains, a human in the loop). `acpx` is containment by authored policy: an untrusted driver gets a read-only window and nothing else. Broader capability is a per-deployment policy you author and own the risk for — not a default.

---

## Verify it

From the bound conversation:

| You ask | Expected |
|---|---|
| read a workspace file | answered |
| list or search the workspace | refused (no shell) |
| run a shell command | refused |
| write or edit a file | refused |
| read `~/.ssh/…` or `.env` | refused |

Reads working while every write, shell, and network action is refused is the profile behaving correctly. If a write or shell action succeeds, the wrong profile is wired — confirm the registered command is `… acpx` (not `acp`) and `nonInteractivePermissions` is `deny`.

---

## Troubleshooting

- **Every prompt fails with a generic error.** `command` is not an absolute path; the systemd-user gateway can't find `contenox`. Use `command -v contenox`.
- **A `codex` harness error.** That message went to OpenClaw's embedded assistant, not Contenox — the conversation isn't bound. Bind it from a channel; webchat cannot bind.
- **Bot ignores you.** `dmPolicy` defaults to `pairing` — approve your pairing code.
- **`/acp spawn` refused.** You are not command owner — set `commands.ownerAllowFrom`.

---

## Where to next

- [The nested permission bomb](/docs/use-cases/nested-permission-bomb/) — why an untrusted driver gets an authored policy, not your keyring.
- [HITL policies](/docs/guide/hitl/) — the acpx policy is an authored file you can tighten.
- [Use from Zed](/docs/integrations/editors/zed/) · [JetBrains](/docs/integrations/editors/jetbrains/) · [AionUi](/docs/integrations/editors/aionui/) — device-owner clients, with interactive approval.

Sources: [ACP agents](https://docs.openclaw.ai/tools/acp-agents) · [Channels](https://docs.openclaw.ai/cli/channels) · [Pairing](https://docs.openclaw.ai/cli/pairing)
