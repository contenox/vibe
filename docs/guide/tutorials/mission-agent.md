---
title: "A mission agent"
description: Declare an agent in a file, fire it at an intent, and walk away. What makes an unattended agent different from an interactive one, why each difference exists, and what it looks like when you get one wrong.
order: 2
---

# Tutorial: a mission agent

In [the vault tutorial](/docs/guide/tutorials/vault-agent/) you fired an agent and
stood there waiting for it. This one you fire and leave:

```bash
contenox mission fire vaultfiler "File every file in inbox/ into the vault as a note" \
  --policy hitl-policy-vault.json --wait
```

```
Mission fired at agent "vaultfiler" under envelope "hitl-policy-vault.json".
```

A **mission** is one intent, one agent, one envelope, run with nobody watching.
That last part changes almost everything about how the agent has to be written,
and none of the changes are optional — each one, left out, produces a mission
that fails in a way the logs describe badly.

This tutorial builds on the vault agent, but the shape applies to any unattended
work.

## What actually changes

Four things, and it is worth knowing why before you write any JSON:

1. **Prose reaches nobody.** An interactive agent ends its turn with a sentence
   you read. An unattended one has no reader. It reports through *tools* or it
   has not reported.
2. **Nobody can answer a question.** An approval prompt with no human in front
   of it is a hang, not a pause.
3. **A failure has to be reported by the agent itself**, because nobody is
   watching the terminal to see it.
4. **The agent needs a name**, because you fire at a name rather than pointing
   at a file.

## 1. The file already has a name

A [declared agent](/docs/guide/agents/) already has a name — its frontmatter
gave it one. The vault agent is a chain you wrote by hand, so it earns its name
the file convention's way instead: discovery registers every `chain-agent-*.json`
file as a dispatchable agent, using the chain's `id` — **not the filename** — as
the agent's name. The vault tutorial already saved its chain as
`.contenox/chain-agent-vaultfiler.json` with `"id": "vaultfiler"`, so there is
nothing to rename here. Confirm it is visible:

```bash
contenox agent list
```

```
ID                                    NAME              SOURCE      KIND    ENABLED
0df94eac-a272-47bd-a044-ad8bb2e4f38c  vaultfiler  discovered  chain   true
```

`discovered` means it came from a file on disk, not from a manual registration.
Rename the file later and the agent keeps its name; change the `id` and you have
renamed the agent. See [chain naming](/docs/guide/chains/naming/).

## 2. Grant the mission toolset

This is the one that catches everyone. Add `mission` to the task's tools:

```json
"tools": ["local_fs", "local_shell", "mission"]
```

Without it the agent can do the work and still fail the mission, because
`mission_report` and `mission_finish` are how a unit speaks at all. The mission
stays `open` forever and the runtime files this against it:

```
[blocker] unit ended two turns without reporting
```

The runtime nudges once — it injects a message telling the unit to reach its
operator — and if the second turn also produces nothing, it gives up and records
that blocker.

## 3. Tell the agent it is alone

Append to your `system_instruction`:

```
You are running UNATTENDED: nobody is reading this conversation, so prose
reaches no one. You reach your operator only through your mission tools.

When every file has a note, call mission.mission_report with a `result` naming
each note you wrote, then call mission.mission_finish with `landed`. If you
cannot finish, call mission.mission_finish with `derailed` or `stuck` and say why.
```

Three verdicts, and they mean different things to whoever reads the record later:
`landed` for done, `derailed` for failed, `stuck` for hit a wall it could not get
past alone. There is also `mission.mission_ask_attention` for a question or a
decision it must not make by itself — [the oracle](#8-optional-let-an-agent-answer-the-routine-asks)
below can sometimes answer those.

**A mission with no `mission_finish` never ends.** It sits at `open` with a
heartbeat that stops advancing.

## 4. Give failures somewhere to go — and denials somewhere else

Two different things go wrong unattended, and they need two different answers.
Getting this backwards is the most expensive mistake in the tutorial, so it is
worth being exact.

### A denial is not a failure

When the envelope refuses a call, the agent gets a refusal *as a normal tool
result* and carries on. No step error is raised, so `on_failure` never fires —
this is a decision, not a fault.

The refusal itself carries the guidance. A categorical deny tells the agent
which envelope decided, and that a workaround is not available:

```
Denied by the active policy hitl-policy-deny-writes.json (rule 4). This is the
envelope refusing the capability, not a transient error and not a judgement
about this particular call. Do not retry it and do not attempt another route to
the same effect. Either continue with the work you can still do, or stop and
report that you are blocked on this.
```

Which is enough on its own. Firing this agent at a deny-writes envelope, with
nothing about denials in its system instruction:

```
Status: stuck  (20s)

[blocker] Unable to write to vault directory
The `local_fs_write_file` operation was denied twice when trying to write to the
`vault/` directory. I have confirmed that the directory exists. I am unable to
proceed with my mission of creating notes in the vault.
```

Twenty seconds and a report you can act on. `compute.maxToolCalls` is the
backstop if an agent ignores the refusal anyway, but a backstop is not a
diagnosis — and a mission that spends its whole tool budget rediscovering one
wall tells you nothing about which wall it was.

### A failure is a step that errored

Model unreachable, a tool that crashed, output the handler could not parse. Those
*do* raise a step error, they *do* end the run, and nothing is reported unless
you route them. Point `on_failure` at a task that reports:

```json
"transition": {
  "on_failure": "report_failure",
  "branches": [ ... ]
}
```

```json
{
  "id": "report_failure",
  "handler": "chat_completion",
  "system_instruction": "The previous step failed. Call mission.mission_report with a `blocker` describing what failed and what you had completed before it, then call mission.mission_finish with `stuck`. Do not retry the work.",
  "execute_config": {
    "model": "{{var:model}}",
    "provider": "{{var:provider}}",
    "tools": ["mission"]
  },
  "transition": { "branches": [ { "operator": "default", "when": "", "goto": "end" } ] }
}
```

The shipped `acp` agent does the same thing with a `recovery.md` per branch and
a root `failure.md`. Copy that shape when the work is worth retrying.

## 5. Write the envelope for a room with nobody in it

Here is the part that is genuinely counter-intuitive, and it is worth being
precise because guessing produces a mission that hangs.

**Two gates run, not one.** The unit is a child process speaking the protocol,
and it evaluates each tool call against its *own* envelope first. When that says
`approve`, it raises a permission request — and the **mission envelope**, the one
you pass to `--policy`, is what answers it.

So an `approve` in the mission envelope is not a pause. It is a question asked
into an empty room. Write the mission envelope with `allow` and `deny` only:

```json
{
  "$schema": "https://contenox.com/schema/hitl-policy-v1.schema.json",
  "default_action": "deny",
  "rules": [
    { "tools": "mission",  "tool": "*",          "action": "allow" },
    { "tools": "local_fs", "tool": "read_file",  "action": "allow" },
    { "tools": "local_shell", "tool": "local_shell", "action": "allow",
      "when": [ { "key": "command", "op": "command_prefix_allowlist", "value": "ls" } ] },
    { "tools": "local_fs", "tool": "write_file", "action": "allow",
      "when": [ { "key": "path", "op": "glob", "value": "**/vault/**" } ] },
    { "tools": "local_fs", "tool": "write_file", "action": "deny" }
  ],
  "compute":   { "maxToolCalls": 40, "maxTokens": 400000, "onExhausted": "finish_stuck" },
  "attention": { "allowAgentAnswers": true, "maxAgentAnswers": 3 }
}
```

Note the first rule: **the mission toolset needs allowing too.** Granting it in
the chain lets the agent see the tools; the envelope decides whether the calls
run.

`compute` is the spend ceiling, and for a mission it is load-bearing rather than
decorative — `maxToolCalls` is counted here and a unit that crosses it ends
`stuck` instead of grinding on unattended.

`attention` decides whether another agent may answer this unit's questions. Leave
it out and the answer is no.

## 6. Fire it

```bash
contenox mission fire vaultfiler "File every file in inbox/ into the vault as a note" \
  --policy hitl-policy-vault.json --wait
```

**`--policy` is required** — pass it, or set `contenox config set
default-mission-policy <file>`. With neither, the fire is refused rather than
guessing at an envelope, which is the right call for something that runs
unsupervised.

**`--wait` is also required.** The unit is a child of this command; when the
command exits the unit dies with it. There is no detached fire from a one-shot
CLI. Fire-and-forget needs a long-lived host — an editor session over
`contenox acp`, using its `/mission` command.

Then read the record:

```bash
contenox mission list
```

```
ID                                    AGENT             ENVELOPE                STATUS   AGE
a1092bea-a498-45cb-8ace-fe6dde43cdca  vaultfiler  hitl-policy-vault.json  landed   1m
```

```bash
contenox mission show <id>       # status, envelope, session, reports
contenox mission reports <id>    # every report in full
contenox inbox list              # reports nobody has read yet
```

The mission record outlives the process that fired it. That is the point of
missions: the terminal you fired from is not where the answer has to be read.

## 7. Check the fence, not the behaviour

A mission that did what you asked proves nothing about containment. Ask for
something the envelope forbids:

```bash
contenox mission fire vaultfiler \
  "Write a file named escape.md in the workspace root, NOT inside vault/. Attempt the write." \
  --policy hitl-policy-vault.json --wait
```

The file is not created. In the trace:

```
hitl "ask raised"      tool=write_file
hitl acp_permission    outcome=selected  option_id=deny
hitl "verdict entered" tool=write_file   approved=false
```

Against the same envelope, a write inside the vault reads `option_id=allow` and
`approved=true`. Same agent, same run shape, opposite verdicts — decided by the
rules in the file, before either call ran.

Test this against a path your `system_instruction` says nothing about. If you ask
for something the prompt already forbids, the model will refuse on its own and
you will have proved nothing about the envelope.

## 8. Optional: let an agent answer the routine asks

When a subagent calls `mission_ask_attention`, or makes a tool call your
envelope put on the `approve` tier, the default is that it waits for a human.
An **oracle** is a reviewer that may rule on the routine ones itself, inside the
`attention` bounds you set in step 5. It is a configured default, not a flag:

```bash
contenox config set default-oracle-chain chain-oracle-default.json
```

That alone lets it answer *questions*. To let it rule on gated *tool calls* as
well, two separate grants have to agree — the host's, and this subagent's
envelope:

```bash
contenox config set oracle-approves-tool-calls true
```

```json
{ "attention": { "allowAgentApprovals": true, "maxAgentApprovals": 10 } }
```

It answers when the mission's own intent already contains the answer:

```
oracle: reviewing attention ask c5dfb1ae (subagent d8e146d2): Confirm filename convention
oracle: answered ask c5dfb1ae in 8.922s: "Use kebab-case for all note filenames."
```

…and refuses when the question is a real decision:

```
oracle: reviewing attention ask 56f0800b (subagent a1092bea): Use Title Case or kebab-case for note filenames?
oracle: WAIT for ask 56f0800b (5.508s) — it stays with a human
```

The difference between those two runs is only the intent. The first said which
convention to use, so the question was already answered and the oracle relayed
it. The second did not, so it stayed with a person and the run waited on the
pending ask.

That is the lesson worth taking from step 8: **the oracle's quality is your
intent's quality.** It judges nothing except against what you wrote. A vague
intent does not get smarter answers — it gets more `wait`s.

Details in [the oracle](/docs/use-cases/auto-attention/).

## When a mission goes wrong

| What you see | What it means |
| --- | --- |
| `[blocker] unit ended two turns without reporting` | The agent never called a mission tool. Either the `mission` toolset is not granted in the chain, not allowed in the envelope, or the system instruction never told it to report. |
| The unit loops, status stays `open`, the same tool keeps being refused | A denial is not a failure and `on_failure` will not catch it. Tell the agent to stop and report on a denial — see [step 4](#4-give-failures-somewhere-to-go--and-denials-somewhere-else). |
| Status stays `open`, heartbeat stops advancing | No `mission_finish`. The work may well have happened — check the vault. |
| `hitl: approval error: context canceled` | Something evaluated to `approve` with nobody to ask. Find the `policy=` field on the `ask raised` line in the trace to see which envelope decided it. |
| Every tool call returns `tool_result_too_large` | The chain has no `token_limit`. See [chain structure](/docs/specification/#chain-structure). |
| `no mission envelope` | No `--policy` and no `default-mission-policy`. |

**Read the trace, not the summary.** The mission record tells you a unit failed;
the tracker output on stderr tells you which tool, which envelope and which
verdict. Capture it:

```bash
contenox mission fire ... --wait > mission.log 2>&1
grep -E 'subject="(ask raised|verdict entered|turn failed)"' mission.log
```

That is the difference between a blocker you can act on and an afternoon of
guessing.

## Next

- [Declaring agents](/docs/guide/agents/) — the one-file road; a declared agent is dispatchable the moment it lands.
- [Missions](/docs/guide/missions/) — sessions, missions and runs compared.
- [The oracle](/docs/use-cases/auto-attention/) — the reviewer in step 8.
- [Chain naming](/docs/guide/chains/naming/) — the file-to-agent rule in full.
- [HITL policies](/docs/guide/hitl/) — the envelope grammar.
