---
title: "Tutorial: your first agent"
description: Declare an agent in one file, watch what contenox puts behind it, and run it. What a permission setting expands into, what a declaration cannot say, and where the knobs are.
---

# Tutorial: your first agent

An agent is a file. This tutorial writes one, shows you the two things contenox
builds behind it, and runs it — because the difference between those two things
is the part worth understanding.

If you already have agents in `.claude/agents/`, skip to
[Agents you already have](#agents-you-already-have). They work as they are.

## Write it

```bash
mkdir -p .contenox/agents
```

`.contenox/agents/reviewer.md`:

```markdown
---
name: reviewer
description: Reviews a Go file for correctness problems
tools: Read, Glob, Grep
---

You are a Go code reviewer. Read the file you are asked about using the tools
you have, then list concrete correctness problems you can point at in the code
you actually read.

Ground every claim: quote the lines you rely on before drawing a conclusion. If
a tool returns nothing or errors, say so and stop rather than guessing. Be
brief.
```

That is the whole agent. No build step:

```bash
contenox agent list
```

```
NAME      SOURCE      KIND   ENABLED
reviewer  discovered  chain  true
```

## Run it

```bash
contenox mission fire reviewer "review payments.go" --wait
```

Or drive it directly while you are iterating:

```bash
contenox run --chain .contenox/.generated/chain-agent-reviewer.json \
  --input-type chat "Review payments.go in the current directory."
```

Reading files is allowed under the default posture, so it runs without asking.
Ask the same agent to write a file and it stops for approval.

## What sits behind it

Your one file became two: a **chain** that says what happens, and a **policy**
that says what is permitted. Your declaration had no way to separate those — it
had one `tools:` list.

Part of the policy:

```json
{
  "default_action": "approve",
  "rules": [
    { "tools": "local_fs", "tool": "*", "action": "deny",
      "when": [{ "key": "path", "op": "glob",
                 "value": "**/{.ssh,.aws,.kube,.config/gcloud}/**" }] },
    { "tools": "local_fs",    "tool": "read_file",   "action": "allow" },
    { "tools": "local_fs",    "tool": "write_file",  "action": "approve" },
    { "tools": "local_shell", "tool": "local_shell", "action": "approve" }
  ]
}
```

The credential deny is added to every agent and comes first, because rules are
first-match-wins.

Add `permissionMode: acceptEdits` to the frontmatter and `write_file` becomes
`allow`; `local_shell` stays `approve`.

You do not maintain these files. Edit the declaration; they follow.

## Agents you already have

Nothing to move or convert:

```
your-project/
  .claude/agents/reviewer.md    ← found where it is
  .contenox/agents/triage.md    ← yours
```

Both are agents. Ones from another tool are prefixed with it
(`claude-code-reviewer`); your own keep their name.

## Connecting a tool

`WebSearch` is not a tool contenox hosts. Name it and the agent runs without
it:

```
not carried  tools: WebSearch    resolves to nothing connected here; the agent
                                 runs without it
```

Connect it as an [MCP server](/docs/guide/mcp/), an OpenAPI spec or a shell
command, then give it the name your declaration already uses:

```toml
# .contenox/agents.toml
[tools]
WebSearch = "tavily.search"
```

It matches no policy rule, so it falls to `default_action` and asks you on
every call. To let it run unattended:

```toml
[[policy.always_allow]]
tools = "tavily"
tool = "search"
```

## The knobs

A declaration says one prompt, one tool list, one model, one permission setting.
Context budget, retries, loop bounds, shell allowlists and compute ceilings live
in [`agents.toml`](/docs/reference/agents-config/), which is commented and
applies to every agent at once:

```toml
[chain]
token_limit = 131072

[tools_policies.local_shell]
_allowed_commands = "ls,cat,git,go"
```

Next: [Declaring agents](/docs/guide/agents/) for the full frontmatter, or
[HITL policies](/docs/guide/hitl/) for the other half.
