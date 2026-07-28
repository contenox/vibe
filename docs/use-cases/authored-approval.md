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

1. Note the active default policy at `~/.contenox/hitl-policy-default.json`. It pauses on filesystem writes, `sed`, shell commands, and mutating HTTP verbs; allows reads and safe HTTP verbs; and fails closed to an approval prompt for anything else (`default_action: "approve"`).

2. Write your own policy as a JSON document — rules are evaluated top to bottom, first match wins:

   ```json
   {
     "default_action": "deny",
     "rules": [
       { "tools": "local_fs",    "tool": "read_file",   "action": "allow" },
       { "tools": "local_fs",    "tool": "list_dir",    "action": "allow" },
       { "tools": "local_fs",    "tool": "write_file",  "action": "approve" },
       { "tools": "local_fs",    "tool": "sed",         "action": "approve" },
       { "tools": "local_shell", "tool": "local_shell", "action": "approve" },
       { "tools": "zendesk",     "tool": "send_reply",  "action": "approve" }
     ]
   }
   ```

   Reading files passes silently, writing files pauses, shell commands pause, sending a Zendesk reply pauses, and everything else is denied.

3. Save it as `~/.contenox/hitl-policy-<name>.json`.

4. Activate it:

   ```bash
   contenox config set hitl-policy-name hitl-policy-<name>.json
   ```

5. Run a chain that calls a gated tool and confirm the approval prompt appears only where your policy said it should.

## Expected outcome

Reads and other `allow` rules run without interruption. Every `approve` rule pauses for a terminal (or editor) approval prompt showing the actual call. Anything not matched by a rule falls through to `default_action`.

## Built-in presets

Contenox ships six policy presets under `~/.contenox/`. Switch between them with `contenox config set hitl-policy-name <file>`.

| Preset | File | Behavior |
|---|---|---|
| Default | `hitl-policy-default.json` | Prompts on writes, `edit_file`, `sed`, shell commands, and mutating HTTP verbs; allows reads and safe HTTP methods; fails closed to approval otherwise. Runs out of the box. |
| Strict | `hitl-policy-strict.json` | Deny-by-default. Plain reads (`read_file`, `list_dir`, `grep`, `web_get`, …) pass silently; every other explicitly listed tool still prompts for approval; anything not listed is denied. For production or compliance-sensitive environments. |
| Dev | `hitl-policy-dev.json` | Allow-all by default — most tool calls pass silently. `local_shell` itself still prompts for approval (with a handful of destructive commands denied outright even here). For local development when you trust the chain and don't want interruptions outside the shell. |
| ACP | `hitl-policy-acp.json` | Transport-specific: editor (ACP) sessions — Zed, JetBrains, AionUi. |
| ACPX | `hitl-policy-acpx.json` | Transport-specific: headless/untrusted-driver sessions (OpenClaw). Deny-by-default with no approval tier. |
| beam | `hitl-policy-beam.json` | Transport-specific: `contenox beam`'s attended terminal session — a copy of the ACP preset tuned for the TUI's own approval card. |

## Where to next

- [HITL policies](/docs/guide/hitl/) — the full policy schema and condition operators.
- [The nested permission bomb](/docs/use-cases/nested-permission-bomb/) — authoring the same boundary for tool credentials, not just approvals.
