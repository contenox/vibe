---
title: HITL Policies
description: Control which tool calls require human approval using named policy presets.
---

# HITL Policies

Human + AI collaboration in contenox is an authored, versioned artifact — not a runtime default. The policy file decides what runs unattended, what pauses to ask a human, and what is denied outright, and it is diffable and swappable like any other file in your repo. Because approvals are durable, a question waits for a person instead of timing out: an unanswered ask checkpoints the run, and answering it later — from any terminal — resumes execution exactly once. A parked turn says so rather than going quiet, and a client that reconnects is shown the question again; see [What a parked approval looks like](#what-a-parked-approval-looks-like). For how these controls fit a sovereignty and oversight posture, see [AI sovereignty & the EU AI Act](/docs/guide/sovereignty/).

Human-in-the-loop (HITL) lets you intercept tool calls before they execute and decide — approve, block, or let them pass automatically — based on a named policy file.

## How it works

HITL is **on by default**. When the engine runs a tool call, it evaluates the active policy and takes one of three actions:

- **approve** — pause and prompt the user; execution continues only after explicit approval
- **allow** — pass through silently (no prompt)
- **deny** — reject the call immediately without prompting

In the **CLI**, approval prompts appear inline in the terminal (TTY):

![A destructive rm command stops at a human approval gate before it runs](/hitl-approve.gif)

To disable HITL entirely, pass `--auto`:

```bash
# HITL on (default) — the engine pauses before destructive tool calls:
contenox chat --shell "refactor main.go"

# HITL off — autonomous mode, no approval prompts:
contenox chat --shell --auto "refactor main.go"
contenox run  --auto --chain ./my-chain.json "do the thing"
```

> **Warning:**
> `--auto` disables all approval prompts. Use only in trusted environments or non-interactive scripts.

## What a parked approval looks like

An approval that nobody answers within the park window does not fail and does not hang. The run checkpoints, releases its process, and the ask stays a pending row any process can answer later. Three things make that state visible rather than silent:

**The turn announces itself.** A parked turn ends with a message in the transcript saying it is suspended rather than finished, naming the approval id and the command that resolves it. On the wire the ACP `stopReason` stays `end_turn` — the protocol has no "suspended" reason, and inventing one would break clients that read it as a closed set — so the distinguishing detail travels in the response's `_meta` alongside the announcement. An ordinary completed turn carries neither.

**A reconnecting client is asked again.** The approval prompt belongs to the connection that raised it, so it disappears when that connection goes. Attaching to the session again — reopening the terminal UI, or an editor reconnecting — re-presents the approvals the session is still parked on, as live prompts under the original ask ids. Answering a re-presented prompt resolves the same durable row and resumes the same checkpointed run; it is not a second question. Asks that were answered elsewhere, or that have expired and had their `on_timeout` verdict applied, are not re-offered. Questions asked with `mission_ask_attention` are not re-offered as prompts either — they are answered with words, through `contenox approvals respond --answer`. The re-offer is capped per attach, so a session holding an unusual number of open asks gets the first of them as cards and the rest stay answerable from a terminal.

**Any terminal can still answer.** None of the above is required. `contenox approvals list` shows every pending ask and `contenox approvals respond` answers one from any terminal, whether or not a client is attached — see [`contenox approvals`](/docs/reference/contenox-cli/#contenox-approvals). Whichever route answers first wins: a row becomes terminal exactly once, so a prompt answered on a second screen after the verdict landed is discarded rather than applied twice.

## Policy file format

A policy is a JSON file with an optional `default_action` and a list of `rules`:

```json
{
  "default_action": "deny",
  "rules": [
    { "tools": "local_fs",    "tool": "write_file",  "action": "approve" },
    { "tools": "local_fs",    "tool": "sed",         "action": "approve" },
    { "tools": "local_shell", "tool": "local_shell", "action": "approve" },
    { "tools": "webtools",    "tool": "web_post",    "action": "approve" },
    { "tools": "webtools",    "tool": "web_put",     "action": "approve" },
    { "tools": "webtools",    "tool": "web_patch",   "action": "approve" },
    { "tools": "webtools",    "tool": "web_delete",  "action": "approve" }
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
> `command_prefix_allowlist` pins a command **name**, and `PATH` decides what a name means — so an allowlisted `go` can be a `go` planted anywhere earlier on `PATH`. Add a `trusted_binaries` block to pin those names to a real path and a SHA256; see [Trusted binaries](/docs/guide/trusted-binaries/).

## Who may answer a unit's question (`attention`)

A mission unit that hits something it must not decide alone calls its
`mission.mission_ask_attention` tool. That question lands in the session
that fired the mission, where you answer it in place — your words go straight
back to the unit as the result of the call it is parked on, and it continues.

By default only a **human** may answer, because that is what the unit escalated
for. An envelope can hand routine questions to the **agent that fired the
mission** instead — it often already knows the answer from the conversation the
mission was fired in:

```json
{
  "default_action": "approve",
  "rules": [],
  "attention": { "allowAgentAnswers": true, "maxAgentAnswers": 2 }
}
```

| Field | Type | Description |
|---|---|---|
| `attention.allowAgentAnswers` | bool | Let the firing session's agent answer its own unit's questions. Omitted/`false` (the default) means a human must. You can always answer yourself either way — this only decides whether the agent is offered the question first. |
| `attention.maxAgentAnswers` | int | How many of this mission's questions the agent may answer before the rest wait for a human. Omitted uses a small default (3); `0` is **not** unlimited. The count is durable and actor-aware, so a restart does not refill it and your own answers do not consume it. |

Together the two fields are a delegation budget: an envelope may allow a bounded
number of agent answers per mission, and once the bound is spent every further
question waits for a human. Every answer — yours or an agent's — durably records
who answered.

The agent is also skipped when the firing session is busy with a turn you
started, or is not currently open — a question is never silently swallowed by an
agent-to-agent exchange you cannot see. When the agent does answer, it happens as
a visible turn in your transcript, and the durable ask records that an agent (not
a person) answered it.

## Built-in presets

Contenox ships seven policy presets, written to `~/.contenox/` by `contenox init`. (A workspace `.contenox/` file with the same name overrides the global one.) The first three are the general-purpose postures; the next three are the profiles the ACP editor transports and the terminal UI load; the last is the pinned envelope of the beta [attention oracle](/docs/use-cases/auto-attention/).

| Name | Behaviour |
|---|---|
| `hitl-policy-default.json` | Prompts for filesystem writes, `sed`, shell commands, and the mutating webtools verbs (`web_post`, `web_put`, `web_patch`, `web_delete`); allows reads (`read_file`, `list_dir`, `grep`, `stat_file`, `count_stats`) and the safe webtools verbs (`web_get`, `web_head`); anything not matched by a rule fail-closes to approval (`default_action: "approve"`) |
| `hitl-policy-strict.json` | Deny-by-default; only the rules listed are prompted |
| `hitl-policy-dev.json` | `default_action: allow`, but explicit rules still gate `local_shell` (every shell call requires approval, and a fixed blacklist is always denied); useful for local development when you don't want prompts on filesystem/webtools calls. Unlike `hitl-policy-default.json` it carries **no secret quarantine** — its only rules cover the shell, so reads of `.ssh`, `.gnupg`, `.aws`, `.azure`, `.kube`, gcloud, browser profiles, wallets, shell history, `.netrc`, `.npmrc` and `id_rsa*` are not denied. Use it on a machine whose secrets you would not mind an agent reading |
| `hitl-policy-acp.json` | Profile for editor (ACP) sessions — gated tool calls route through the editor's own approval UI |
| `hitl-policy-acpx.json` | Hardened profile for headless / untrusted-driver (ACPX, e.g. OpenClaw) sessions — shell, writes, and network are denied outright rather than offered for approval |
| `hitl-policy-beam.json` | the terminal UI's default envelope — a copy of `hitl-policy-acp.json` tuned for the attended terminal UI: approve-tier writes and shell commands surface as a one-keypress card in the transcript rather than an editor approval dialog |
| `hitl-policy-oracle.json` | the [attention oracle's](/docs/use-cases/auto-attention/) pinned envelope — `default_action: deny` with exactly two allows, the in-process `oracle.submit_verdict` and `oracle.verdict_state` tools; no shell, filesystem, or network rule of any kind; seeded by init, inert until `mission fire --oracle` mounts the driver |

Each preset also states who may answer a unit's question (see [`attention`](#who-may-answer-a-units-question-attention)) rather than inheriting the invisible default, and the stances follow each preset's character:

| Name | `attention` |
|---|---|
| `hitl-policy-acp.json` | agent may answer, up to 3 — an editor session's agent holds the conversation the mission was fired in |
| `hitl-policy-beam.json` | agent may answer, up to 3 — same stance as `hitl-policy-acp.json`, for the terminal UI's attended session |
| `hitl-policy-default.json` | agent may answer, up to 3 — the mission path's default envelope: routine questions (a session agent's, or the attention oracle's), while whatever the unit then *does* stays gated by this same envelope; the file's `//attention` note documents the grant |
| `hitl-policy-dev.json` | agent may answer, up to 5 — the permissive local-development posture |
| `hitl-policy-strict.json` | **human only** — a policy whose character is "a human decides" does not hand the deciding to a model |
| `hitl-policy-acpx.json` | **human only** — an untrusted driver's agent must not answer its own subagent's escalation |
| `hitl-policy-oracle.json` | **human only** — the oracle never adjudicates its own asks; a question the oracle chain raises waits for a human |

## Selecting the active policy

```bash
contenox config set hitl-policy-name hitl-policy-strict.json
contenox config get hitl-policy-name   # verify
```

This writes to the KV store and takes effect immediately — no restart required. The setting applies to all subsequent invocations in the same workspace.

## Policy resolution order

When HITL is enabled and a tool call needs evaluation, the engine resolves the policy as follows:

1. Read the `hitl-policy-name` key from the KV store.
2. If set, load that file from the workspace `.contenox/` directory, falling back to `~/.contenox/`.
3. If the key is empty or the file is missing, fall back to `hitl-policy-default.json`.
4. If that file is also missing, use a built-in fail-closed policy with no rules: every tool call, including reads, requires approval.

A policy decides which tool calls need a human. It does not decide **where** the session may run — that is the workspace-root allowlist, assembled from the launch directory, the roots you granted, and this run's flags. See [Workspace roots](/docs/reference/contenox-cli/#workspace-roots) for how the one allowlist is built and [`contenox workspace`](/docs/reference/contenox-cli/#contenox-workspace) for granting and revoking the durable half of it.
