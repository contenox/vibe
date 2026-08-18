---
title: The pause is yours to define
description: HITL isn't a checkbox. It's a policy file you wrote.
---

# The pause is yours to define

Write a HITL (human-in-the-loop) policy that pauses only on the tool calls you name, then activate it.

## Prerequisites

- `contenox` installed and initialized in a project (`contenox init`).
- A chain that calls tools — the default chain qualifies.

## Steps

1. Read the active default at `~/.contenox/.generated/hitl-policy-default.json`. It pauses on filesystem writes, `sed`, and shell commands; allows reads; and fails closed to an approval prompt for anything else (`default_action: "approve"`). That file is **transpiled** from `[envelopes.default]` in `agents.toml` and rewritten on every run — read it, do not edit it.

2. Write your own policy as a JSON document — rules are evaluated top to bottom, first match wins:

   ```json
   {
     "default_action": "deny",
     "rules": [
       { "tools": "local_fs",    "tool": "read_file",   "action": "allow" },
       { "tools": "local_fs",    "tool": "write_file",  "action": "approve" },
       { "tools": "local_fs",    "tool": "sed",         "action": "approve" },
       { "tools": "local_shell", "tool": "local_shell", "action": "approve" },
       { "tools": "zendesk",     "tool": "send_reply",  "action": "approve" }
     ]
   }
   ```

   Reading files passes silently, writing files pauses, shell commands (including directory listing and search, which now go through `local_shell`) pause, sending a Zendesk reply pauses, and everything else is denied.

3. Save it as `~/.contenox/hitl-policy-<name>.json` — the **top level**, not `.generated/`. A file there sits ahead of the transpiled envelopes on the search path and nothing rewrites it.

   Writing the same thing as an envelope instead is usually less work, because the axes carry the credential quarantine and the write wall for you:

   ```toml
   # agents.toml
   [envelopes.support]
   extends = "ask_always"
   default_action = "deny"

   [envelopes.support.tools]
   "zendesk.send_reply" = "approve"
   ```

4. Activate it:

   ```bash
   contenox config set hitl-policy-name hitl-policy-<name>.json
   ```

5. Run a chain that calls a gated tool and confirm the approval prompt appears only where your policy said it should.

## Expected outcome

Reads and other `allow` rules run without interruption. Every `approve` rule pauses for a terminal (or editor) approval prompt showing the actual call. Anything not matched by a rule falls through to `default_action`.

## Shipped envelopes

Contenox ships its envelopes as `[envelopes.<name>]` sections in `agents.toml`, transpiled into `.generated/hitl-policy-<name>.json` on every run. Switch between them with `contenox config set hitl-policy-name <file>`, or per run with `--hitl-policy <name>`.

| Envelope | File | Behavior |
|---|---|---|
| `default` | `hitl-policy-default.json` | Prompts on writes, `edit_file`, `sed`, and shell commands; allows reads; fails closed to approval otherwise. The floor every surface lands on. |
| `strict` | `hitl-policy-strict.json` | Deny-by-default, and the shell loses its allowlist tier, so every command line asks — including `ls`. For runs where you want everything to ask first. |
| `acpx` | `hitl-policy-acpx.json` | For a driver you did not write: headless/untrusted sessions (OpenClaw). Deny-by-default with no approval tier. |
| `read_only` / `ask_always` / `auto_edit` | `hitl-policy-<name>.json` | The three postures a declaration's `permissionMode` resolves through. |
| `oracle` | `hitl-policy-oracle.json` | The [oracle's](/docs/use-cases/auto-attention/) pinned envelope: the `oracle.*` toolset and nothing else. |
| `serve` | `hitl-policy-serve.json` | The MCP host: missions and the servers you connected, nothing local. |

There is no `dev` and no `acp` envelope — `acp` folded into `default`, and only envelopes that mean something ship. If you want the older permissive local-development posture, write one:

```toml
[envelopes.dev]
extends = "auto_edit"
description = "Local development: edits land, the shell still asks."
default_action = "allow"
```

## Where to next

- [HITL policies](/docs/guide/hitl/) — the full policy schema and condition operators.
- [`[envelopes.<name>]`](/docs/reference/agents-config/#envelopesname) — the axis grammar you write one in.
- [The nested permission bomb](/docs/use-cases/nested-permission-bomb/) — authoring the same boundary for tool credentials, not just approvals.
