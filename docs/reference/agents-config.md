---
title: agents.toml
description: The config file behind agent declarations — budgets, loop bounds, shell allowlists, how a permission setting widens into rules, and what each tool name resolves to.
---

# `agents.toml`

An [agent declaration](/docs/guide/agents/) says one prompt, one tool list, one
model and one permission setting. Everything else lives here.

It is TOML rather than JSON because it exists to be read and argued with, and
JSON cannot carry the comments that make a value arguable.

## Where it lives, and which wins

It is read from each root, weakest first:

1. the defaults compiled into the binary
2. `~/.contenox/agents.toml`
3. `.contenox/agents.toml` — the workspace, which wins
4. `[agents.<name>]` in either file — one agent, overriding the rest

Then, stronger than any of them, the values a declaration actually states. And
stronger than everything, `policy.always_deny`, which nothing overrides.

**Overlays are partial.** A file may set any subset; keys it omits keep the
value inherited from the layer below. Overriding one knob does not mean
restating the file, and naming one tool does not drop the built-in names.

A missing file is skipped. A malformed one is an error rather than a silent
fall back to shipped values — an operator who edited a file and got the
defaults anyway has been lied to.

---

## `[agents.<name>]` — one agent at a time

Everything under `[chain]`, `[routing]`, `[tools_policies]` and `[policy]`
applies to every agent. Nest it under `[agents.<name>]` and it applies to one:

```toml
# Everyone gets a large budget and a broad shell.
[chain]
token_limit = 131072

# The reviewer reads; it has no business running anything but git and go.
[agents.reviewer.chain]
token_limit = 32768

[agents.reviewer.tools_policies.local_shell]
_allowed_commands = "git,go"
```

`<name>` is the name [`contenox agent list`](/docs/reference/contenox-cli/)
shows. Your own declarations keep the name their frontmatter gave them; an
agent read out of another tool's directory carries that tool's prefix, so a
`reviewer.md` in `.claude/agents/` is `[agents.claude-code-reviewer]`.

The same partial-overlay rule applies, one level down: a key the section omits
keeps what it inherited. That includes zero and `false` — `retry_on_failure = 0`
under one agent genuinely means zero even when the root says three.

Three things behave differently inside a per-agent section, all so a narrow
override cannot quietly widen anything:

- **`tools_policies` merges per knob.** Naming one shell knob keeps the rest of
  the shell policy and every other toolset.
- **`postures` merges per posture.** Redefining `auto_edit` leaves `read_only`
  and `ask_always` as the root declared them.
- **`always_deny` and `always_allow` append after the root's**, never replace
  them. First match wins in the emitted policy, so the root's credential denies
  keep their position ahead of anything an agent grants itself.

A section naming an agent that does not exist is **reported, not ignored** — a
mistyped name otherwise reads as a knob that does nothing.

Editing this file regenerates every agent it affects on the next run; you do
not have to touch the declaration.

---

### `[chain]`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `token_limit` | int | `131072` | Context budget for chat history, and the source of the per-call tool-result cap. **Must be positive** — a zero budget reports every tool result as too large regardless of its real size. |
| `max_tokens` | string | `"{{var:max_tokens\|16384}}"` | Output cap per model call. [Macros](/docs/guide/concepts/#macros) are honoured, so a caller overrides it per run without editing the chain. |
| `think` | string | `"{{var:think}}"` | Reasoning effort. A declaration's own `effort` overrides this; the vocabularies coincide. |
| `main_rounds` | int | `60` | Edge traversals before the main stage hands to recovery. **Must be positive.** |
| `recovery_rounds` | int | `10` | Traversals before recovery summarises the failure rather than looping. **Must be positive.** |
| `retry_on_failure` | int | `0` | Per-task retries, for transient provider errors. |

### `[routing]`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `model` | string | `"{{var:model}}"` | Emitted model. |
| `provider` | string | `"{{var:provider}}"` | Emitted provider. |
| `pin_model` | bool | `false` | Use the model the declaration itself names instead of the templates, when the registry resolved it. |

Templates are the default deliberately: an agent written against one vendor
must not pin this machine to that vendor — routing stays whatever
`contenox config set default-provider` says. The model the declaration named is
kept in provenance either way. Turn it on for a single agent that genuinely
needs a specific model with `[agents.<name>.routing]`.

### `[tools_policies.<toolset>]`

Free-form key/value passed straight into the emitted chain's
`execute_config.tools_policies`. A declaration has no word for these, which is
the main reason this file exists.

Only the toolsets an agent actually exposes are carried into its chain, so an
agent does not carry policy for tools it cannot reach.

Shipped knobs:

```toml
[tools_policies.local_shell]
_allowed_commands = "ls,cat,echo,pwd,which,find,grep,git,go,python3,node,npm,make,cargo,curl,wget,jq"
_denied_commands = "sudo,su,dd,mkfs,fdisk,parted,shred"

[tools_policies.local_fs]
_allowed_dir = "."
_max_read_bytes = "262144"
_max_output_bytes = "131072"
_denied_path_substrings = "node_modules,.git/,dist/,/.next/,/out/,package-lock.json"
```

Add a table for any toolset you connect; see [Tools](/docs/integrations/tools/)
for the keys a given toolset reads.

### `[policy]`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `default_action` | `allow` \| `approve` \| `deny` | `approve` | Applied to a tool call no rule matched — including any tool you mapped yourself. |

### `[policy.compute]`

| Key | Type | Default |
|---|---|---|
| `max_tool_calls` | int | `300` |
| `max_tokens` | int | `2000000` |
| `on_exhausted` | string | `finish_stuck` |
| `max_turns` | int | `0` |

A declaration that states a turn cap (`maxTurns`) lowers
`max_tool_calls` when it is tighter, and never raises it. The two are not the
same unit — a turn may carry several calls — so the mapping only tightens.

`max_turns` applies to a mission-role agent only, and only two values mean
anything, because the drive loop issues at most two prompts — the unit's own,
plus one nudge when it reports nothing. `0` keeps the nudge; `1` drops it.
Anything larger is already above the ceiling and is **not emitted**, since a
bound the runtime cannot honour would read as enforced while doing nothing.
A primary agent never gets one: its turns are the operator's own prompts.

`max_tool_calls` is validated but not enforced by the shipped hosts.
`max_tokens` is best-effort and inert when a provider reports no usage.
`on_exhausted` supports only `finish_stuck`.

### `[[policy.always_deny]]`

An array of tables emitted **first**, under every posture, in every emitted
policy. Rules are first-match-wins, so their position is what makes them
effective.

```toml
[[policy.always_deny]]
tools = "local_fs"
tool = "*"
when_key = "path"
when_op = "glob"
when_value = "**/{.ssh,.aws,.kube,.config/gcloud}/**"
```

| Key | Meaning |
|---|---|
| `tools` | toolset name, or `*` |
| `tool` | tool name, or `*` |
| `when_key` / `when_op` / `when_value` | optional condition; `when_op` takes any [policy operator](/docs/guide/hitl/) — `glob`, `eq`, `host`, `command_prefix_allowlist`, and the rest |

No declaration can waive these: the format has no way to talk about credential
paths, so it has no way to consent to them.

### `[[policy.always_allow]]`

The mirror of `always_deny`, for tools the postures do not name — typically ones
you connected. Emitted **after** the denies, so first-match-wins keeps a
credential deny ahead of any grant here.

```toml
[[policy.always_allow]]
tools = "tavily"
tool = "search"
```

Without it, a tool you named under `[tools]` matches no rule and falls to
`default_action`, which asks a human on every call.

### `[policy.postures.<name>]`

How a declaration's single permission setting widens into rules. Three postures are
required and validated: `read_only`, `ask_always`, `auto_edit`.

| Key | Value |
|---|---|
| `local_fs_read` | `allow` \| `approve` \| `deny` |
| `local_fs_write` | `allow` \| `approve` \| `deny` |
| `local_shell` | `allow` \| `approve` \| `deny` |

```toml
[policy.postures.auto_edit]
local_fs_read = "allow"
local_fs_write = "allow"
local_shell = "approve"
```

Shipped mapping from declaration settings:

| Source | Posture |
|---|---|
| Cursor `readonly: true` | `read_only` |
| Claude Code `default` / `manual`, Antigravity default | `ask_always` |
| Claude Code / Antigravity `acceptEdits`, Claude Code `auto` | `auto_edit` |
| Claude Code `plan`, `dontAsk` | `read_only`, with a note in the loss report |
| Claude Code / Antigravity `bypassPermissions` | **refused.** Use `acceptEdits` and grant what the agent needs below, where the grant is written down |

Loosening a posture here loosens it for every agent that uses it. `auto_edit`
granting `local_shell = "allow"` would mean an agent whose source asked only to
accept edits gets a shell.

### `[naming]`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `scope_with_dialect` | bool | `true` | Prefix chain ids from *other tools* directories with the product they came from, so two tools' identically named agents do not collide. Your own declarations are never scoped. |

### What a declaration registers for itself

`agents.toml` maps names onto tools **you** connected. A declaration can also
bring its own MCP servers and OpenAPI services — see
[Tools an agent brings with it](/docs/guide/agents/#tools-an-agent-brings-with-it).
Those are a different tier and this file does not describe them:

| | Registered by | Scope | Retired when |
|---|---|---|---|
| `contenox mcp add` / `contenox tools add` | you | every agent | you remove it |
| `mcpServers:` / `remoteTools:` in a declaration | the declaration | that one agent | the declaration is deleted |

A declaration-scoped registration is named `decl-<agent>-<name>` and is
deliberately **not** reachable by `tools: ["*"]` from any other agent — a
wildcard means every tool this machine offers, and another agent's private
source is not that. It also never appears in `contenox mcp list` output as
something you own: reconciliation only ever touches rows carrying that prefix.

`[tools]` below still applies to both: it is how a declaration's tool *names*
resolve, whoever registered the thing behind them.

### `[tools]`

What each tool name a declaration may use resolves to, as `toolset.tool`.

```toml
[tools]
WebSearch = "tavily.search"
```

Overlays merge, so naming one tool leaves the built-in names alone.

**A mapping makes a tool reachable, not permitted.** The emitted policy carries
rules for the tools contenox hosts; a name you mapped yourself matches none of
them and falls to `default_action` — so it works, and asks a human on every
call. Give it a rule in the emitted policy, or in `[policy.postures.*]` above to
cover every agent.

Two kinds of name are absent from the shipped table for different reasons:

- **Tools nobody has connected** — `WebSearch`, `NotebookEdit`. contenox ships
  no tool catalog. Connect the tool as an [MCP server](/docs/guide/mcp/), an
  OpenAPI spec or a shell command, then add a line here.
- **Host capabilities that do not exist** — `Skill`, `TodoWrite`, `Task`,
  `manage_task`. No mapping can supply these; they are not tools.

Names containing a dot are treated as MCP tools and pass through unchanged, so
MCP needs no entries at all.

Canonical tools contenox hosts:

| Toolset | Tools |
|---|---|
| `local_fs` | `read_file` `write_file` `edit_file` `sed` `read_file_range` |
| `local_shell` | `local_shell` |

### `[models]`

What a declaration's model name resolves to, as `provider:model`.

```toml
[models]
sonnet = "anthropic:claude-sonnet-5"
flash = "gemini:gemini-2.5-flash"
```

Only consulted when `pin_model` is on. An unrecognised name is not an error:
routing stays templated and the raw name is kept in provenance.

---

## Worked example

Run every agent read-only, with a tighter shell:

```toml
# .contenox/agents.toml
[chain]
token_limit = 65536

[tools_policies.local_shell]
_allowed_commands = "git,go"

[policy]
default_action = "deny"

[naming]
scope_with_dialect = false
```

Everything not named here — loop bounds, postures, the credential denies —
keeps its shipped value.
