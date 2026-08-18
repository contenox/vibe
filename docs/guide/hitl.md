---
title: HITL Policies
description: Control which tool calls require human approval. You write an envelope in agents.toml; the runtime transpiles it into the policy the approval engine evaluates.
order: 9
---

# HITL Policies

Human + AI collaboration in contenox is an authored, versioned artifact — not a runtime default. The policy decides what runs unattended, what pauses to ask a human, and what is denied outright, and it is diffable and swappable like any other file in your repo.

You do not normally write that policy by hand. You write an **envelope** — a named `[envelopes.<name>]` section in [`agents.toml`](/docs/reference/agents-config/#envelopesname) — and the runtime transpiles it into the JSON below. An envelope transpiles to a policy; the approval engine is unchanged either way. See [Where a policy comes from](#where-a-policy-comes-from). Because approvals are durable, a question waits for a person instead of timing out: an unanswered ask checkpoints the run, and answering it later — from any terminal — resumes execution exactly once. A parked turn says so rather than going quiet, and a client that reconnects is shown the question again; see [What a parked approval looks like](#what-a-parked-approval-looks-like). For how these controls fit a sovereignty and oversight posture, see [AI sovereignty & the EU AI Act](/docs/guide/sovereignty/).

The file format has a published JSON Schema, generated from the Go types that load it: [`hitl-policy-v1.schema.json`](/schema/hitl-policy-v1.schema.json). Add it as `$schema` in your policy file and your editor validates as you type; every transpiled envelope and every emitted per-agent policy already carries it. The chain format is at [`task-chain.schema.json`](/schema/task-chain.schema.json).

Human-in-the-loop (HITL) lets you intercept tool calls before they execute and decide — approve, block, or let them pass automatically — based on a named policy file.

## How it works

HITL is **on by default**. When the engine runs a tool call, it evaluates the active policy and takes one of three actions:

- **approve** — pause and prompt the user; execution continues only after explicit approval
- **allow** — pass through silently (no prompt)
- **deny** — reject the call immediately without prompting

In the **CLI**, approval prompts appear inline in the terminal (TTY).

To disable HITL entirely, pass `--auto` on a surface that would otherwise prompt:

```bash
# HITL on (default) — the engine pauses before gated tool calls:
contenox acp

# HITL off — autonomous mode, no approval prompts:
contenox acp --auto
contenox events dispatch --auto
```

> **Warning:**
> `--auto` disables all approval prompts. Use only in trusted environments or non-interactive scripts.

## What a parked approval looks like

An approval nobody has answered yet does not fail and does not hang. The run checkpoints, releases its process, and the ask stays a pending row any process can answer later. Three things make that state visible rather than silent:

**The turn announces itself.** A parked turn ends with a message in the transcript saying it is suspended rather than finished, naming the approval id and the command that resolves it. On the wire the ACP `stopReason` stays `end_turn` — the protocol has no "suspended" reason, and inventing one would break clients that read it as a closed set — so the distinguishing detail travels in the response's `_meta` alongside the announcement. An ordinary completed turn carries neither.

**A reconnecting client is asked again.** The approval prompt belongs to the connection that raised it, so it disappears when that connection goes. Attaching to the session again — reopening beam, or an editor reconnecting — re-presents the approvals the session is still parked on, as live prompts under the original ask ids. Answering a re-presented prompt resolves the same durable row and resumes the same checkpointed run; it is not a second question. Asks that were answered elsewhere, or that have expired and had their `on_timeout` verdict applied, are not re-offered. Questions asked with `mission_ask_attention` are not re-offered as prompts either — they are answered with words, through `contenox approvals respond --answer`. The re-offer is capped per attach, so a session holding an unusual number of open asks gets the first of them as cards and the rest stay answerable from a terminal.

**Any terminal can still answer.** None of the above is required. `contenox approvals list` shows every pending ask and `contenox approvals respond` answers one from any terminal, whether or not a client is attached — see [`contenox approvals`](/docs/reference/contenox-cli/#contenox-approvals). Whichever route answers first wins: a row becomes terminal exactly once, so a prompt answered on a second screen after the verdict landed is discarded rather than applied twice.

## Where a policy comes from

An **envelope** is a permission surface with a name. It states what a session may
reach — files, shell, network, missions, the tools you connected — in a
vocabulary of capability axes, and the runtime compiles it into the rules below.
Nothing about the evaluation engine changes; the envelope is only the vocabulary
the rules are written in.

```toml
# ~/.contenox/agents.toml
[envelopes.review]
extends = "read_only"
description = "Read the tree, run the test suite, change nothing."

[envelopes.review.shell]
grant = "deny"
prefix_allowlist = ["go test", "go vet"]
```

**The name is the whole identity.** Envelope `review` transpiles to
`hitl-policy-review.json`, and `--hitl-policy review`,
`--hitl-policy hitl-policy-review.json` and
`config set hitl-policy-name hitl-policy-review.json` all resolve to it. The
filename rule is exactly `"hitl-policy-" + name + ".json"` — the same rule
per-agent policies follow, so the two families share one namespace and a
collision between a declared agent and an envelope is a startup error rather
than a silent overwrite.

The rendered file lands in `.generated/`, is stamped with the section it came
from, and is rewritten on every run:

```
~/.contenox/.generated/hitl-policy-review.json
```

That directory is derived and disposable. Edit the envelope, not the render —
and if you want a file of your own, write
`~/.contenox/hitl-policy-review.json` at the top level instead, where the search
path puts it ahead of the render and nothing rewrites it. [`contenox vet`](/docs/reference/contenox-cli/#contenox-vet-path)
says so when it finds one, because a shadowed render means editing `agents.toml`
changes nothing.

The full axis grammar — the seven axes, their refinements, `extends`, and the
order the rules come out in — is in
[`[envelopes.<name>]`](/docs/reference/agents-config/#envelopesname).

> **Migrating:** an existing `hitl-policy-*.json` keeps winning. Nothing rewrites
> the top-level copies earlier builds seeded into `~/.contenox/`, and while one is
> there it is the file that gates your runs. Delete it to fall through to the
> envelope, or keep it and ignore the envelope of that name.

## Policy file format

This is what an envelope compiles to, and what the engine loads. Read it when you
want to know what a rendered envelope actually did, or when you are writing a
policy file by hand.

A policy is a JSON file with an optional `default_action` and a list of `rules`:

```json
{
  "default_action": "deny",
  "rules": [
    { "tools": "local_fs",    "tool": "write_file",  "action": "approve" },
    { "tools": "local_fs",    "tool": "edit_file",   "action": "approve" },
    { "tools": "local_fs",    "tool": "sed",         "action": "approve" },
    { "tools": "local_shell", "tool": "local_shell", "action": "approve" }
  ]
}
```

| Field | Type | Description |
|---|---|---|
| `default_action` | `"allow"` \| `"approve"` \| `"deny"` | Action for tool calls that match no rule. Fail-closes to `"approve"` if omitted — an unaccounted-for tool call pauses for a human rather than running. |
| `rules[].tools` | string | Tools name (`local_fs`, `local_shell`, a remote tool name, …) |
| `rules[].tool` | string | Tool name within that tool (`write_file`, `sed`, `local_shell`, …) |
| `rules[].action` | `"approve"` \| `"allow"` \| `"deny"` | What to do when this rule matches |
| `rules[].when` | array | Optional conditions on the call's arguments; **all** must hold for the rule to match (AND). Each is `{ "key": …, "op": …, "value": … }`. Omit for a name-only match. |
| `rules[].timeout_s` | int | Seconds to wait for a human response when `action` is `approve`. `0` (default) waits indefinitely until the context is cancelled. |
| `rules[].on_timeout` | `"approve"` \| `"deny"` | Fallback action when an approval window expires. `"allow"` is rejected (it would silently bypass approval). |

Rules are evaluated top-to-bottom; the first match wins.

An expiry is applied when the ask is next read rather than by a background
sweep — contenox runs no daemon — so a verdict arriving after the window still
resumes the run until something lists the inbox.

Of the compute bounds, `maxTokens` applies when a unit's provider reports usage
and is inert when it does not, and `maxToolCalls` is validated at parse time and
enforced by the unattended permission answerer. Which bound is enforced how is
tabulated in [Compute bounds](/docs/guide/missions/#compute-bounds-and-what-actually-enforces-them).

### Condition operators (`when[].op`)

| Op | Matches |
|---|---|
| `eq` | Argument value equals `value` exactly |
| `glob` | Argument value (a path) matches a glob pattern; supports `*`, `?`, and `**` |
| `host` | Argument value, parsed as a URL, has a host equal to or a subdomain of one of the comma-separated hosts in `value` |
| `command_blacklist` | Command basename is in the comma-separated denylist in `value` (also catches every command in a compound shell line, where readable) |
| `command_ask_always` | Same match as `command_blacklist`, for pairing with `action: "approve"` instead of `deny` |
| `no_command_substitution` | Command line contains shell substitution syntax (`$()`, backticks, `<()`, `>()`) |
| `command_prefix_allowlist` | Command line, as tokens, starts with one of the comma-separated safe prefixes in `value` (e.g. `"git log"` covers `git log --oneline` but not `git clean -fd`); refuses to match any call using shell mode or containing a control/substitution character |

A rule with `when` conditions gates a tool only for calls whose arguments match — for example, prompting only for shell commands in a blacklist:

```json
{ "tools": "local_shell", "tool": "local_shell", "action": "approve",
  "when": [{ "key": "command", "op": "command_ask_always", "value": "rm,sudo,dd,chmod" }] }
```

> **Note:**
> `command_prefix_allowlist` pins a command **name**, and `PATH` decides what a name means — so an allowlisted `go` can be a `go` planted anywhere earlier on `PATH`. Add a `trusted_binaries` block to pin those names to a real path and a SHA256; see [Trusted binaries](/docs/guide/confinement/trusted-binaries/).

## Who may answer a subagent (`attention`)

A subagent that hits something it must not decide alone calls its
`mission.mission_ask_attention` tool. That question lands in the session
that fired it, where you answer it in place — your words go straight
back to the subagent as the result of the call it is parked on, and it continues.

The same block also decides who may rule on the subagent's **gated tool calls**:
a call the envelope put on the `approve` tier normally waits for you, and this
is where you say whether an adjudicating agent may answer it instead.

By default only a **human** may do either, because that is what the subagent
escalated for. An envelope can hand routine questions to the **agent that fired
the mission** — it often already knows the answer from the conversation the
mission was fired in — and can separately let an **oracle** rule on gated calls:

```json
{
  "default_action": "approve",
  "rules": [],
  "attention": {
    "allowAgentAnswers": true,
    "maxAgentAnswers": 2,
    "allowAgentApprovals": true,
    "maxAgentApprovals": 10
  }
}
```

| Field | Type | Description |
|---|---|---|
| `attention.allowAgentAnswers` | bool | Let the firing session's agent, or the oracle, answer this subagent's questions. Omitted/`false` (the default) means a human must. You can always answer yourself either way — this only decides whether an agent is offered the question first. |
| `attention.maxAgentAnswers` | int | How many of this mission's questions an agent may answer before the rest wait for a human. Omitted uses a small default (3); `0` is **not** unlimited. The count is durable and actor-aware, so a restart does not refill it and your own answers do not consume it. |
| `attention.allowAgentApprovals` | bool | Let the oracle approve or deny this subagent's `approve`-tier **tool calls**. Omitted/`false` (the default) means every gated call waits for a human. Separate from `allowAgentAnswers` on purpose: letting a model answer a question is a smaller grant than letting it release a gated call. |
| `attention.maxAgentApprovals` | int | How many of this mission's gated calls the oracle may decide before the rest wait for a human. Omitted uses a default (20); `0` is **not** unlimited. Durable and actor-aware, same as the answer budget. |

In an envelope this block is the `missions.answer` axis, which is the one axis
whose carrier is not a rule — the mission toolset is exempt from approval, so
delegation is read from `attention` instead:

```toml
[envelopes.mine]
missions.answer = "allow"     # an agent may answer questions
# missions.answer = "approve" # …and may also rule on gated calls
# missions.answer = "deny"    # human only; the block is omitted entirely

[envelopes.mine.attention]
max_agent_answers = 2
```

An explicit `[attention]` key wins over what the axis would set, so the axis
states the stance and the block puts a number on it.

Together these are a delegation budget: an envelope may allow a bounded number
of agent answers and agent-decided calls per mission, and once a bound is spent
every further ask waits for a human. Every verdict — yours or an agent's —
durably records who gave it.

An adjudicating agent can only ever be **faster** than you, never more
permissive than the envelope. It cannot widen a rule, cannot reach a tool the
policy denies, and cannot exceed these counts; the counting and the write happen
in one statement, so a mix of answerers cannot overrun the budget even
concurrently. Anything it declines to decide takes the untouched normal path and
waits for you.

The agent is also skipped when the firing session is busy with a turn you
started, or is not currently open — a question is never silently swallowed by an
agent-to-agent exchange you cannot see. When the agent does answer, it happens as
a visible turn in your transcript, and the durable ask records that an agent (not
a person) answered it.

## Shipped envelopes

Contenox ships eight envelopes in `agents.toml`, transpiled into `.generated/` on
every run. Nothing seeds a `hitl-policy-*.json` any more — a name resolves
because the envelope behind it was rendered, not because a copy of a preset was
written where your own file goes.

Only envelopes that mean something ship. There is no one-to-one carry-over of the
old preset filenames: `dev` is gone, and `acp` folded into `default`, which
absorbed its two extra shell tokens (`>:` and `>>`).

The first three are the postures a [declaration's](/docs/guide/agents/) permission
setting resolves through; the rest are the profiles a surface runs under.

| Envelope | Renders to | Behaviour |
|---|---|---|
| `read_only` | `hitl-policy-read_only.json` | Read the workspace, change nothing. The base of the chain, and where the **credential quarantine** rides: key stores, keyrings, wallets, browser profiles and shell history are denied on every `local_fs` tool, ahead of every grant, under the most permissive posture exactly as under the strictest |
| `ask_always` | `hitl-policy-ask_always.json` | Read freely, ask before changing anything. Adds the **write wall** — anything that *runs* something (a shell rc, a systemd unit, a LaunchAgent, `.git/`, an approval policy) is denied outright rather than offered, because approving one is approving everything it will later do unattended |
| `auto_edit` | `hitl-policy-auto_edit.json` | Edits land without asking; the shell still asks. `auto_edit` means edits, not execution |
| `default` | `hitl-policy-default.json` | The floor, and **not optional**: every surface that names no envelope lands here, and so does every name that fails to resolve. `ask_always` with the shell spelled out in five tiers and with ceilings on what one detached mission may spend |
| `strict` | `hitl-policy-strict.json` | Refuse what is not named; ask about everything that is. `default_action: deny`, and the shell loses its allowlist tier entirely, so every command line asks — including `ls` |
| `acpx` | `hitl-policy-acpx.json` | The envelope for a driver you did not write. It differs from `strict` on one word: deny, not ask. Writes and the shell are refused rather than offered, and secret paths are refused **on read** rather than escalated — asking about a file is already telling the asker the file is there |
| `oracle` | `hitl-policy-oracle.json` | The [oracle's](/docs/use-cases/auto-attention/) pinned envelope, and the only pure allowlist in the set: `default_action: deny` with the in-process `oracle.*` toolset allowed and nothing else. Inert until `default-oracle-chain` mounts the driver |
| `serve` | `hitl-policy-serve.json` | [`contenox serve`](/docs/guide/serve/), stated structurally rather than tightened: `files.read`, `files.write` and `shell` all deny, because a standing host mounts no filesystem and no shell at all. What does arrive is the MCP servers you connected, so `default_action` stays `approve` — nothing here can know what they do |

Each envelope also states who may answer a unit's question (see
[`attention`](#who-may-answer-a-subagent-attention)) rather than inheriting the
invisible default, and the stances follow each envelope's character:

| Envelope | `missions.answer` |
|---|---|
| `default` | agent may answer, up to 3 — the mission path's floor: routine questions go to a session agent or the oracle, while whatever the subagent then *does* stays gated by this same envelope |
| `serve` | agent may answer, up to 3 — a host has nobody sitting at it, and the questions it raises are about tools you connected |
| `strict` | **human only** — an envelope whose character is "a human decides" does not hand the deciding to a model |
| `acpx` | **human only** — an untrusted driver's agent must not answer its own subagent's escalation |
| `oracle` | **human only**, on both halves — the oracle never adjudicates its own asks; a question the oracle chain raises waits for a human |

The three postures state no `missions.answer` of their own: a posture describes
what an agent may *do*, and who may answer for it is the profile's business.

## Selecting the active policy

Per surface, for one run:

```bash
contenox beam --hitl-policy strict                       # an envelope by name
contenox acp  --hitl-policy hitl-policy-strict.json      # the same envelope
contenox serve ~/src/api --hitl-policy ./ops/locked.json # a file, used verbatim
```

Persistently, for a workspace:

```bash
contenox config set hitl-policy-name hitl-policy-strict.json
contenox config get hitl-policy-name   # verify
```

The config key writes to the KV store and takes effect immediately — no restart
required. It applies to all subsequent invocations in the same workspace.

## Policy resolution order

Each surface resolves its envelope at startup, in three steps:

1. **`--hitl-policy <name-or-path>`.** A value carrying a path separator is a
   path and is honoured verbatim — that exact file, and a missing one is an error
   rather than a fallback. Anything else is an envelope name, and both
   `strict` and `hitl-policy-strict.json` name the same one.
2. **Your own file.** A top-level `hitl-policy-<name>.json` in the workspace
   `.contenox/`, then `~/.contenox/`, wins if it is there.
3. **The transpiled envelope**, from `.generated/hitl-policy-<name>.json`.

So the search path, strongest first, is:

```
.contenox/                    your workspace copy
~/.contenox/                  your global copy
.contenox/.generated/         the workspace's rendered envelopes
~/.contenox/.generated/       the global rendered envelopes
```

Rendering is **self-healing**: every declared envelope is re-transpiled on every
run, exactly as the profile's chain is. A render that fails while a readable copy
is already on the path is reported and survived — a stale envelope still gates,
an absent one does not. Only a name that resolves to nothing at all is fatal, and
it names both what it looked for and where.

Within a run, a tool call is then evaluated against the resolved policy. A
workspace with `hitl-policy-name` set uses that name; empty or unresolvable falls
back to `hitl-policy-default.json`, which is why the `default` envelope is not
removable. If even that is missing, the engine uses a built-in fail-closed policy
with no rules: every tool call, including reads, requires approval.

A policy decides which tool calls need a human. It does not decide **where** the session may run: one instance serves one workspace, fixed when the instance was launched, and no policy widens or narrows it. See [Workspace authority](/docs/reference/contenox-cli/#workspace-authority).
