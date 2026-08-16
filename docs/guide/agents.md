---
title: Declaring agents
description: An agent is a Markdown file with a YAML frontmatter header. Where declarations live, what the frontmatter says, what the config file supplies that a declaration cannot, and what an agent is allowed to do.
---

# Declaring agents

An agent is one file:

```markdown
---
name: reviewer
description: Reviews a file for correctness problems
tools: Read, Glob, Grep
---

You are a code reviewer. Read the file you are asked about, then list the
problems you can point at in what you actually read.
```

The frontmatter says how to run it, the body becomes its system prompt. Drop it
in and the next run picks it up.

If you have written agents for Claude Code, this is the same file.

## Where declarations live

```
.contenox/
  agents/
    reviewer.md      one agent
    triage.md        another
  agents.toml        what a declaration cannot say
```

`~/.contenox/agents/` works the same way for agents you want everywhere.

`.claude/agents/` and `.agents/agents/` in your project are read where they
are. Those agents are prefixed with the tool they came from (`reviewer` becomes
`claude-code-reviewer`); your own keep their name.

## The frontmatter

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | the agent's identity |
| `description` | yes | when to reach for it |
| `tools` | no | the tools it may call. **Omitted inherits every tool** — name them to narrow it |
| `model` | no | routing stays on your configured default unless you pin it |
| `permissionMode` | no | `acceptEdits` auto-accepts file writes; otherwise writes and shell ask you first |
| `effort` | no | reasoning effort: `low`, `medium`, `high`, `xhigh` |
| `maxTurns` | no | tightens how much one run may spend |
| `mcpServers` | no | MCP servers this agent may reach, or ones it brings itself — see [Tools an agent brings with it](#tools-an-agent-brings-with-it) |
| `remoteTools` | no | OpenAPI services this agent brings itself. contenox's own field; a file without it is unchanged |

The body is the system prompt and expands the usual macros — `{{tools}}`,
`{{host:os}}`, `{{var:…}}`, `{{date}}` — plus [`{{skills}}`](#skills-procedures-for-repeated-work).

`memory`, `isolation` and `color` are reported as not carried. Three others are
reported with what replaces them: `hooks` (the runtime governs those events —
see below), `skills` (a directory the agent reads), and `background` (already
how a dispatched agent runs).

Tool names resolved out of the box: `Read`, `Write`, `Edit`, `Bash`,
`PowerShell`, `Glob`, `Grep`, `WebFetch`. An unknown name is dropped and
reported; the agent runs with the rest. A declaration where no tool resolves
fails.

## Using it

Declared agents are ordinary agents. They appear in the roster, in an editor's
`/mission` list, and to the planner:

```bash
contenox agent list
contenox mission fire reviewer "review the payment retry change" --wait
```

## Skills: procedures for repeated work

A skill is a Markdown file describing how to do a recurring job — which tools to
call, in what order, what to show the human, where to file the result. Put them
beside your agents:

```
.contenox/
  agents/
    office.md
  skills/
    timesheet.md          one procedure
    release/SKILL.md      or a folder, when it ships reference files
```

```markdown
---
name: timesheet
description: File this week's hours to the timesheet system
---

Read the tracked hours from the time tool, present the week for approval,
submit the approved rows, then file a confirmation note.
```

Pull the inventory into an agent with `{{skills}}`:

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

**The index, not the bodies.** Only the one-line description costs context; the
agent reads the file with `local_fs.read_file` when a request matches — the same
call, under the same policy rules, logged where every other read is logged. Ten
procedures cost ten lines, not ten documents.

The macro expands when the chain is generated, not per request, so the prompt
stays a stable cache prefix. Add or edit a skill and the next pass rewrites the
agents that use it.

Frontmatter is optional — a bare Markdown file works, taking its name from the
filename and its description from the first line.

> Skills are read relative to the project, so they live in the workspace
> `.contenox/skills/`. One in `~/.contenox/skills/` is not listed: the agent's
> file tool is rooted at the project and refuses absolute paths, so an entry it
> cannot open would be an instruction that fails.

**Skill or agent?** Both work for a job like a timesheet. A skill loads into the
agent you are already talking to and keeps the conversation's context; an agent
is a separate actor with its own session and envelope, dispatched with
`mission fire`. Reach for a skill when the procedure should come up mid-task,
and an agent when it is a job you start.

## Tools an agent brings with it

Most agents use tools you connected once and share across all of them. An agent
that needs something of its own can carry it in its declaration instead.

### Naming servers you already registered

A list under `mcpServers` is a **grant**: this agent may reach these
[MCP servers](/docs/integrations/tools/mcp/), and nothing else new.

```yaml
mcpServers: [github, linear]
```

This is Claude Code's own shape, and it means the same thing here.

### Bringing a server or service

A **mapping** under `mcpServers` defines servers rather than naming them, and
`remoteTools` does the same for any [OpenAPI service](/docs/integrations/tools/remote/):

```yaml
---
name: researcher
description: Researches a question against internal sources
mcpServers:
  filesystem:
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/data"]
  linear:
    type: http                       # or sse
    url: https://mcp.linear.app/mcp
    authEnvKey: LINEAR_TOKEN         # the variable's name, never its value
remoteTools:
  billing:
    url: https://internal.example.com
    spec: https://internal.example.com/openapi.json
---
```

These are registered **scoped to this agent**. Two agents may each bring a
`filesystem` without colliding, no other agent can reach either one — not even
one that inherits every tool — and deleting the declaration retires what it
brought.

They show up in `contenox mcp list` and `contenox tools list` under an
`OWNER` of `declaration`, because they are genuinely running on this machine:

```
NAME                        TRANSPORT  COMMAND/URL  OWNER
github                      http       …            you
decl-researcher-filesystem  stdio      npx          declaration
```

Anything you registered yourself is never touched by a declaration. The
reverse also holds: editing or removing a `declaration`-owned row by hand does
not stick — the next discovery pass writes it back from the file.

### Credentials stay out of the file

A declaration is committed to source control, so it may not carry a literal
credential. Name the environment variable instead:

```yaml
    authEnvKey: LINEAR_TOKEN     # accepted
    authToken: sk-live-abc123    # refused, with the file and field named
```

For a server that needs an interactive login, register it once with
[`contenox mcp add`](/docs/reference/contenox-cli/) and `contenox mcp auth`,
then **name** it from the declaration. Registration can live in a file;
a browser OAuth round-trip cannot.

### What this means for a shared repository

`.claude/agents/` is read out of your workspace, so an agent declaration can
arrive with a `git clone` or a merged pull request — and a declaration with a
stdio `command` starts that process when the agent it belongs to is registered.
contenox does not second-guess this: the file is in your tree and you chose to
run the agent. Treat a declaration the way you treat a `Makefile` or a
`package.json` script, and read one before you run an agent from a repository
you do not control.

## Tools you connect

contenox hosts `local_fs` and `local_shell`, both forwarded to your ACP
client's `fs/*` and `terminal/*` capabilities. Everything else you connect as
an [MCP server](/docs/integrations/tools/mcp/), an OpenAPI spec, or a shell command
(`local_shell` reaches `git`, search tools, and anything else your shell can
run).

A declaration naming `WebSearch` keeps its other tools and reports the drop:

```
not carried  tools: WebSearch    resolves to nothing connected here; the agent
                                 runs without it. Connect it and name it under
                                 [tools] in agents.toml
```

Connect the tool, then give it the name your declarations already use:

```toml
# .contenox/agents.toml
[tools]
WebSearch = "tavily.search"
```



## Naming a tool is not permitting it

Every tool call is checked against a policy before it runs. That policy has
rules for the tools contenox hosts. It has no rule for `tavily.search`, so a
newly connected tool falls through to `default_action` — `approve` — and asks
you on every call.

To let it run unattended, give it a rule in `agents.toml`:

```toml
[[policy.always_allow]]
tools = "tavily"
tool = "search"
```

Two rules apply to every agent and cannot be overridden:

- Filesystem access to `.ssh`, `.aws`, `.kube` and gcloud config is denied under
  every permission setting.
- `permissionMode: bypassPermissions` is refused.

## Branching: the directory is the chain

One declaration is one loop: a turn, its tools, and a bounded second attempt.
When a request needs *different* loops — changing code is not the same job as
reviewing it — you do not reach for a different format. You make directories.

```
agents/
  triage/
    agent.md          the classifier: which branch handles this?
    code/
      agent.md        one loop
      recovery.md     its second attempt (optional)
    docs/
      agent.md        another loop
```

`contenox` reads that as **one agent** called `triage`. The `agent.md` beside
the subdirectories becomes a router; each subdirectory becomes a branch; every
leaf is the ordinary five-task loop a single declaration already emits. Nesting
works without any further idea — a branch that itself branches is just a
directory with children.

### The label is the directory name

The router does not list its branches. It cannot: they are the directory names,
and `contenox` appends them to your classifier prompt along with each branch's
`description`, so the model is told exactly which answers are valid.

This is the point of the convention. In a hand-written chain the prompt names
its labels in prose while the transitions match the same strings by equality,
and nothing keeps the two in step — rename a branch and it silently stops being
reachable. Here there is one string, so there is nothing to drift.

### The default is required

```markdown
---
name: triage
description: Send a request to the branch that should handle it.
default: docs
---

You sort an incoming request. Read it and answer with one label, nothing else.
```

A classifier answering something unmapped is ordinary, so `default:` says where
that goes. It is refused if it names no branch — routing an unsorted request to
whichever directory sorted first is how work ends up in the wrong loop with
nothing saying so. A router with exactly one branch needs no `default:`.

If the classifier itself *fails* — the model behind it is briefly unreachable —
the request also takes the default branch. The work is still doable, it is just
not sorted.

### recovery.md is present or absent, never a flag

A recovery prompt is a different prompt: it is written for an agent that has
already failed once. So it is a file, and a branch that should simply give up
omits it — an exhausted loop then goes straight to the failure report.

### failure.md

At the root of a tree, `failure.md` is what the chain says when every branch has
given up. One per tree, because there is one report; which branch was running is
already in the transcript.

### Telling the agent how much budget is left

A recovery prompt usually wants to say how far the turn has got. Two things it
must not do: name a task, or state a number.

```markdown
You have used {{rounds_used}} of {{main_rounds}} main and
{{recovery_rounds_used}} of {{recovery_rounds}} recovery rounds.
```

`contenox` resolves those when it emits the chain. The counters become live
edge counts over *this* leaf's own loop, so you never write a task id and
renaming the directory cannot break the prompt. The budgets become the numbers
from `agents.toml`, so a prompt cannot promise a budget the chain does not
enforce — which the hand-written chains did, claiming twelve main rounds while
enforcing sixty.

### What still lives in agents.toml

Loop bounds, budgets and tool policy are not declarations — see below. The
classifier runs on `router_model` / `router_provider`, which default to your
ordinary model: choosing a lane is a one-word answer and rarely needs the model
the lane itself will use.

```toml
[routing]
router_model = "{{var:alt_model|var:model}}"

[agents.triage.chain]
recovery_rounds = 8
```

### The shipped agents are declarations

`contenox init` seeds `triage/` as a worked example — and the agents contenox
runs for itself are the same thing. `acp` is a tree (router, plus coding,
general, and review leaves); `acpx` is a single flat declaration. They are
written to `~/.contenox/agents/` and transpiled into `.generated/` at init, so
the agent answering you is authored the way this page tells you to author
yours, and you can read and edit it. Both exist to demonstrate the authoring
convention and give a working default, not as a product catalogue — the
operator brings their own agents beyond these.

Four chains are still shipped as JSON, because a declaration does not describe
them: `compact` and `fim` are single-task chains with no tool loop, and
`planner` and `oracle` carry stages — a settle check, an early exit — with no
counterpart in a declaration. Converting them would mean inventing behaviour.

## What a declaration cannot say

Context budgets, retries, loop bounds and shell allowlists live in
[`agents.toml`](/docs/reference/agents-config/) beside your declarations.

By default a value there applies to every agent:

```toml
# .contenox/agents.toml
[chain]
token_limit = 131072
```

Nest it under `[agents.<name>]` and it applies to one — the name is the one
`contenox agent list` shows:

```toml
[agents.reviewer.chain]
token_limit = 32768

[agents.reviewer.tools_policies.local_shell]
_allowed_commands = "git,go"
```

Keys you leave out keep the value from the layer below, so a per-agent section
is only the difference, never a restatement. Edit the file and the next run
picks it up; you do not touch the declaration.

A section naming an agent that does not exist is reported rather than ignored,
so a typo does not read as a knob that does nothing.

## Checking before anything runs

[`contenox vet`](/docs/reference/contenox-cli/) validates what a declaration
became — handler signatures, dataflow, rule shapes, transitions that can never
fire:

```bash
contenox vet
```
