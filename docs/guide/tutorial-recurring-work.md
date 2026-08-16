---
title: "Tutorial: a recurring job"
description: "Build one real repeating job — filing a timesheet — and learn the whole declaration format on the way: what an agent file is, how it brings its own tools, where the approval step lives, and when a procedure should be a skill instead of an agent."
---

# Tutorial: a recurring job

[Your first agent](/docs/guide/tutorial-first-agent/) gets one file running in
five minutes. This one takes a job you actually repeat and builds it properly,
because that is where the format's shape becomes clear.

The job: **file this week's timesheet.** Read the tracked hours, show them to a
human, submit the approved rows, note that it was filed. Four steps, three of
them involving a tool, one of them requiring a person to say yes.

By the end you will have used every part of the format that matters.

---

## What the format is

An agent is a Markdown file with a YAML frontmatter header. The frontmatter says
how to run it; the body is its system prompt.

Five products converged on that same file independently — Claude Code, Google
Antigravity, GitHub Copilot, OpenCode, Cursor. contenox reads it because it is
the file people already write, not because it is ours. If you have written a
Claude Code subagent, you have already written one of these.

What none of those products carry is what happens *after* the file: they run the
agent in a session you are watching. contenox runs it as a governed unit that
can work while you are not there. Same file, different back half.

---

## 1. The agent

```markdown
---
name: timesheet
description: Files this week's tracked hours to the timesheet system
tools: read_file, write_file
---

You file weekly timesheets.

Read the tracked hours for the current week. Present the week as a table —
date, project, hours, note — and wait for the person to approve it. Submit only
the rows they approved. When the submission succeeds, write a one-line
confirmation to `reports/timesheet-<week>.md`.

If the hours do not add up or a project code is missing, stop and say so
rather than guessing.
```

Save it as `.contenox/agents/timesheet.md`. That is the whole registration — no
build step, no restart:

```bash
contenox agent list
```

```
NAME       SOURCE      KIND   ENABLED
timesheet  discovered  chain  true
```

The `tools:` line names what it may call. **Omit it and the agent inherits every
tool** — name them to narrow it. The names resolved out of the box are the local
ones — `read_file`, `write_file`, `edit_file`, `sed`, `read_file_range` (all
under `local_fs`) and `local_shell` — plus anything you have registered as a
remote tool or MCP server.

---

## 2. The tools it does not have yet

`read_file` cannot reach a timesheet system. That lives behind an internal
API, and the agent needs it.

You have two ways to give it one, and the difference is worth understanding.

**Name a tool you already connected.** If you registered the service once with
[`contenox tools add`](/docs/integrations/tools/remote/) or
[`contenox mcp add`](/docs/integrations/tools/mcp/), every agent can reach it,
and a declaration names it to opt in:

```yaml
mcpServers: [timesheet-api]
```

**Or let the agent bring its own.** A mapping instead of a list defines the
server; `remoteTools` does the same for any OpenAPI service:

```markdown
---
name: timesheet
description: Files this week's tracked hours to the timesheet system
remoteTools:
  timesheet:
    url: https://timesheet.internal.example.com
    spec: https://timesheet.internal.example.com/openapi.json
---
```

Every operation in that spec becomes a callable tool. The registration is
**scoped to this agent** — no other agent can reach it, not even one that
inherits every tool, and deleting the file retires it. It shows in
`contenox tools list` with an `OWNER` of `declaration`, so you can tell it from
the ones you registered yourself.

> **Credentials never go in the file.** A declaration is committed to source
> control, so a literal token is a parse error naming the field. Name an
> environment variable instead (`authEnvKey: TIMESHEET_TOKEN`), or register the
> server once with `contenox mcp add` and `contenox mcp auth` — a browser login
> cannot live in a file — and then just *name* it from the declaration.

---

## 3. The approval step is not a prompt

Look at what the body says: *"wait for the person to approve it."*

That is an instruction, and an instruction is a probability. If the approval
actually matters — and on a timesheet it does — it belongs in the policy, not
the prompt:

```toml
# .contenox/agents.toml

[agents.timesheet.tools_policies.local_shell]
_allowed_commands = "git"

[[agents.timesheet.policy.always_allow]]
tools = "timesheet"
tool = "get_hours"
```

Anything the policy does not explicitly allow falls through to
`default_action`, which is `approve` — it asks a human on every call. So reading
hours runs unattended, and **submitting them stops and waits**, because you
never granted it.

That is the part the prompt cannot do. The model can be talked out of an
instruction; it cannot be talked past a rule.

The `[agents.timesheet.…]` prefix scopes those settings to this one agent —
everything under `[chain]`, `[routing]`, `[tools_policies]` and `[policy]` works
the same way, globally or per agent. Keys you leave out keep the value from the
layer below, so a per-agent section is only the difference. See
[`agents.toml`](/docs/reference/agents-config/) for the full set.

---

## 4. Running it

Three ways, same agent:

```bash
contenox mission fire timesheet "file this week" --wait
```

From an editor over ACP, `/mission` lists it. From the web app on your phone,
you attach to the session and answer the approval there — which is the point of
the approval living in the policy: the gate can wait for you.

`--wait` is required from the CLI. The dispatched unit is a child of that
process, so a detached fire from a one-shot command would tear down its own
work. A long-lived host — an editor session — can fire and walk away.

---

## 5. When it should be a skill instead

Now the question that trips everyone.

You could run this as `mission fire timesheet`. You could also write it as a
**skill** — a procedure file the agent you are already talking to reads when the
topic comes up:

```markdown
<!-- .contenox/skills/timesheet.md -->
---
name: timesheet
description: File this week's hours to the timesheet system
---

Read the tracked hours, present the week for approval, submit the approved
rows, then write the confirmation note.
```

An agent that wants the catalogue writes `{{skills}}` in its body:

```markdown
---
name: office
description: Handles recurring office work
---

You handle recurring work.

{{skills}}
```

which becomes, in that agent's prompt:

```
Skills are procedures for repeated work. When a request matches one, read its
file before starting, then follow it.

- timesheet: File this week's hours to the timesheet system — read .contenox/skills/timesheet.md
```

Only the one-line description costs context. The agent reads the file with
`local_fs.read_file` when a request matches — same call, same policy rules,
logged where every other read is logged. Ten procedures cost ten lines.

**So which one?** The difference is one axis:

|  | Agent | Skill |
|---|---|---|
| Who starts it | you dispatch it | the model reaches for it, mid-task |
| Context | its own session | the current agent's |
| Model, tools, policy | its own | the host agent's |
| How many at once | one | many |

An agent is a **separate actor**. A skill is **reference material the current
actor picks up**.

For a timesheet, either works — and `mission fire timesheet` versus asking an
office agent to file your hours is the same gesture when *you* are choosing.
Reach for a skill when the procedure should come up in the middle of a
conversation about something else. Reach for an agent when it is a job you
start, or when it needs its own tools, its own model, or its own envelope.

> Skills are read relative to the project, so they live in the workspace
> `.contenox/skills/`. A flat `timesheet.md` works; a `timesheet/SKILL.md`
> folder works too, and is what lets a skill ship reference files beside its
> instructions. Frontmatter is optional — a bare Markdown file takes its name
> from the filename and its description from the first line.

---

## If you came from Claude Code

Most of the file carries straight across. `name`, `description`, `tools`,
`disallowedTools`, `model`, `permissionMode`, `maxTurns`, `effort` and
`mcpServers` all mean what they mean over there. `.claude/agents/` is read where
it lies, prefixed with the tool it came from, so nothing needs moving.

Three fields are reported as **replaced** rather than missing, and the
replacement is the interesting part:

| Field | What contenox does instead |
|---|---|
| `hooks` | Governed in the runtime, not by running shell commands. Tool gating is the policy; notifications are attention asks and the inbox; stop conditions are the drive loop; context is the system prompt and its macros. Only `PostToolUse` has no equivalent. |
| `skills` | A directory the agent reads — `.contenox/skills/` plus `{{skills}}`, as above. No loading machinery, because a skill only instructs and the instructing acts through tool calls that are already gated. |
| `background` | Already the default. A dispatched agent runs detached and reports back; there is nothing to switch on. |

`memory`, `isolation` and `color` are genuinely not carried and say so.

The one that surprises people is `hooks`. A hook is a shell command run at a
lifecycle point, and it exists because a local process with no execution model
has no other way to make something deterministic. contenox has an execution
model, so the same needs land as policy rules, chain tasks and the drive loop —
and an ungoverned shell command at a lifecycle point is exactly what the policy
engine exists to prevent.

---

## What you built

```
.contenox/
  agents/
    timesheet.md        the agent
    office.md           an agent that reaches for procedures
  skills/
    timesheet.md        the procedure
  agents.toml           budgets, allowlists, who has to approve what
```

Four files, no JSON. Behind them contenox generated a chain that says what
happens and a policy that says what is permitted — you can read both under
`.contenox/.generated/`, and you never edit them: change a declaration and they
follow.

Read them when you want to see exactly what an agent may do. Write one yourself
when you need something a declaration cannot say — a branch, a different model
per step, a recovery path, a step that must happen whether or not the model
agrees. That is [writing a chain by hand](/docs/guide/first-chain/), and it is
the next tier down, not a different product.
