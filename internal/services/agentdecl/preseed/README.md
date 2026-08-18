# Agents

Every `.md` file here is one agent. The format is Markdown with a YAML
frontmatter header: the frontmatter says how to run it, the body becomes its
system prompt.

```markdown
---
name: reviewer
description: Reviews a file for correctness problems
tools: Read, Bash
---

You are a code reviewer. Read the file you are asked about, then list the
problems you can point at in what you actually read.
```

Drop a file in, and the next run picks it up. There is no build step, and
nothing to compile.

## Frontmatter

| Field | Meaning |
|---|---|
| `name` | required; the agent's identity |
| `description` | required; when to reach for it |
| `tools` | tools it may call — `Read`, `Write`, `Edit`, `Bash`. A name with a dot is an MCP tool and reaches whatever server you attached. Omitted means none |
| `model` | optional; routing stays on your configured default unless you pin it |
| `permissionMode` | optional; `acceptEdits` auto-accepts file writes, otherwise writes and shell ask you first |

Anything else is reported rather than silently dropped — the run says what it
could not carry, and why.

## What the format cannot say

A declaration states one prompt, one tool list, one model and one permission
setting. Everything else — context budget, retry behaviour, shell allowlists,
what a permission setting expands into — lives in `../agents.toml`,
which is commented and yours to edit.

## Subagents

Every agent here is also a **subagent**: `/plan` and the `mission_start` tool
dispatch it as one, and `contenox mission fire` fires it by name. `researcher.md`
is the worked example — it plans, reports, and finishes with a verdict, which is
what a subagent has instead of a reply you can read.

A subagent runs unattended, so two things it never needs as a chat agent become
load-bearing:

- **Its mission tools are the only channel.** `mission_report` to say something,
  `mission_ask_attention` to ask for a decision and wait, `mission_plan` to keep
  a reviewable plan, `mission_finish` to end with a verdict. Say so in the body,
  as `researcher.md` does — a subagent that answers in prose reaches nobody.
- **Its envelope is a budget, not just a gate.** Turn, token and tool-call
  ceilings come from `../agents.toml`, and so does whether an agent may answer
  its asks instead of a human. Both default to off.

## Agents you already have

Files in `.claude/agents/` and `.agents/agents/` are read from here too. You do
not have to move or convert them.
