---
description: Give a model controlled, policy-scoped access to the filesystem and shell on the machine contenox runs on.
---

# Local Tools

Contenox never touches the filesystem or spawns processes itself. Local tools give a model access to the filesystem and shell of the machine a client is running on: `local_fs` and `local_shell` calls are forwarded to the connected client's `fs/*` and `terminal/*` capabilities, which the client — `contenox beam`, or an editor — actually carries out.

A [`contenox serve`](/docs/guide/serve/) host has no such client, so neither of these toolsets exists there: every capability a host has is an MCP server or an OpenAPI service you attached.

## `local_fs` — Filesystem access

Forwarded to the ACP client's `fs/*` capability. Provides read, write, and edit operations scoped to a configured directory. **All paths are validated** against the allowed directory; attempts to escape with `../` are rejected.

The filesystem root comes from the ACP session's workspace context.

`tools_policies.local_fs` controls read/output limits and denied path substrings, and can override the root for a specific task with `_allowed_dir`.

Directory listing, searching, and globbing are not part of this toolset — ACP defines no `fs/list` or `fs/grep` method to forward them through. Use [`local_shell`](#local_shell--shell-command-execution) (`ls`, `find`, `grep`/`rg`) for those instead.

### Tools

| Tool | Parameters | Description |
|---|---|---|
| `read_file` | `path` | Read the full content of a file. Also satisfies the read-before-mutate prerequisite for `write_file` / `edit_file` / `sed` against the same path. |
| `write_file` | `path`, `content` | Write content to a file (creates parent dirs, overwrites). For *existing* files, requires a prior full `read_file` against the same current file version in this session. |
| `edit_file` | `path`, `old_string`, `new_string`, `replace_all` (optional) | Replace an exact, byte-for-byte occurrence of `old_string` with `new_string` in an existing file — the targeted alternative to `write_file`'s full overwrite. See [`edit_file`](#edit_file-exact-string-replacement) below. |
| `sed` | `path`, `pattern`, `replacement` | Replace a literal string in a file (not regex). For existing files, requires a prior `read_file` or `read_file_range` of the same path in this session. |
| `read_file_range` | `path`, `start_line`, `end_line` | Read a specific line range. Satisfies targeted `edit_file` / `sed` mutations, but not full-file `write_file` overwrites. |

### `edit_file`: exact-string replacement

`edit_file` replaces one exact occurrence of `old_string` with `new_string` in an existing file, without resending the whole file — the preferred tool for a targeted change, cheaper and safer than `write_file`'s full overwrite.

- **Byte-exact and unique.** `old_string` must match the file's current on-disk text exactly, whitespace included. By default it must occur **exactly once**; if it matches zero times the file is left unchanged and the model is told to re-read and retry with the exact current text, and if it matches more than once the call is refused with a count so the model can add surrounding context to make it unique — a fuzzy or ambiguous match is never applied.
- **`replace_all`.** Set `replace_all: true` to replace every occurrence instead of requiring exactly one (e.g. renaming an identifier throughout the file).
- **Read-before-write.** Same contract as `sed`: a prior `read_file` or `read_file_range` of the current file version in this session is required before `edit_file` may run; the file's hash is re-verified immediately before writing, and a change since the read (by anyone) refuses the edit rather than clobbering it.
- Returns compact JSON (`path`, `written`, `replacements`, `old_bytes`, `new_bytes`, `old_sha256`, `new_sha256`) — not the full file bodies.

### Read-before-write contract

`write_file` against an *existing* file is blocked unless the same session has previously called full `read_file` on that exact current file version. A line-range read is not enough for full-file overwrite because unseen content could be destroyed.

`edit_file` and `sed` are targeted mutators, so either `read_file` or `read_file_range` against the same path can satisfy their prerequisite. New files (paths that do not yet exist) are unaffected.

The model receives a soft denial it can act on: it sees a tool result instructing it to read the file first, then retry the mutation.

This is a deterministic guard — no LLM judgement involved — designed to prevent confabulated edits to files the model has never seen. The contract is scoped per session: a read in one `contenox session` does not satisfy a write in another. The state lives in a private `local_fs_reads` table the tool maintains itself; the chain engine has no visibility into it.

If the model uses `local_shell` (`cat`, `head`, `grep`, `sed`) instead of `local_fs.read_file`, the guard does *not* count it as a satisfying read — by design. The shell tools are not bounded the same way and broadening the guard to recognise their output reliably is impractical. Prefer `local_fs.*` tools for file inspection (the default chains include a `TOOL PREFERENCE` system-prompt addendum that nudges the model toward this).

### Approval diff

When a `write_file`, `edit_file`, or `sed` call is gated to `approve` by the active [HITL policy](/docs/guide/hitl/), the approval prompt carries a unified diff of the exact change, not just the raw tool arguments: HITL independently re-reads the file's current on-disk contents (bypassing this session's read-dedup cache, so the diff is never built from a stale copy) and computes the prospective new contents by replaying the same mutation the tool would make, then renders a unified diff (±3 lines of context, capped at 500 file lines / 120 diff lines) for the human to review before approving. A new file (no prior content) shows as an addition. If the current contents cannot be established safely, the ask is still shown, without a diff.

### `tools_policies.local_fs` keys

Set per-task read/output limits and denied path substrings by adding a `tools_policies.local_fs` block to `execute_config`:

| Key | Type | Default | Description |
|---|---|---|---|
| `_allowed_dir` | path | registration root | Override the allowed filesystem root for this task. Relative paths resolve against the active workspace/cwd where available. |
| `_max_read_bytes` | int | `1048576` (1 MiB) | Max file size for a whole-file `read_file`. `0` or negative = unlimited. Larger files return an error so the model can narrow with `read_file_range`. |
| `_max_output_bytes` | int | `32768` (32 KiB) | Max byte size of any tool result returned to the model. `0` or negative = unlimited. Prefer setting `_model_context_tokens` (below) over overriding this directly. |
| `_model_context_tokens` | int | unset | When set (and `_max_output_bytes` is not), derives the output cap as a fraction of the model's context window instead of using the fixed default. |
| `_denied_path_substrings` | comma-sep | empty | Path substrings that always reject (e.g. `node_modules,.git/,dist/`). Matched against the path relative to the allowed root. |
| `_verbose_tool_descriptions` | bool-string | `false` | Restore the long-form tool descriptions (truncation semantics, did-you-mean suggestions, the read-before-write contract) for large-context models. Off by default to save tokens on every turn. |

```json
"tools_policies": {
  "local_fs": {
    "_allowed_dir": ".",
    "_max_read_bytes": "1048576",
    "_max_output_bytes": "32768",
    "_denied_path_substrings": "node_modules,.git/,dist/,/.next/,/out/,package-lock.json"
  }
}
```

Values are strings even when conceptually numeric — `tools_policies` is the chain's policy carrier and uses string values uniformly across tools. The default chains ship with conservative limit, root, and deny-substring defaults.

### Chain example

```json
"execute_config": {
  "model": "qwen3:8b",
  "provider": "ollama",
  "tools": ["local_fs"]
}
```

---

## `local_shell` — Shell command execution

> **Caution:**
> `local_shell` gives the model direct access to run arbitrary commands on the machine the ACP client is running on. **Never enable it in public-facing deployments or when processing untrusted user input.**

`local_shell` is forwarded to the ACP client's `terminal/*` capability, governed by HITL policy — there is no CLI flag that turns it on or off. It's also where directory listing, searching, and globbing now live (`ls`, `find`, `grep`/`rg`), since `local_fs` no longer has tools for those.

**Command policy is a file, not a CLI flag.** For a declared agent it lives in [`agents.toml`](/docs/reference/agents-config/), globally or under `[agents.<name>]`:

```toml
[tools_policies.local_shell]
_allowed_commands = "git,go,make,ls,cat"
_denied_commands  = "sudo,su,dd,mkfs"
```

In a chain you author yourself it is a `tools_policies` block on `execute_config`, per task:

```json
"execute_config": {
  "model": "{{var:model}}",
  "provider": "{{var:provider}}",
  "tools": ["local_shell"],
  "tools_policies": {
    "local_shell": {
      "_allowed_commands": "git,go,make,ls,cat",
      "_denied_commands":  "sudo,su,dd,mkfs"
    }
  }
}
```

- `_allowed_commands` — comma-separated list of permitted command names. When set, any command not on this list is rejected before it runs.
- `_denied_commands` — comma-separated commands that are always blocked, regardless of the allowlist.
- `_allowed_dir` — if set, the command executable or script path must reside under this directory.

The default chains ship with sensible defaults: common dev tools allowed, privilege-escalation and raw-disk commands denied.

To use `local_shell` with **no policy restrictions** (fully open), omit `tools_policies` entirely. Only do this in fully trusted, local-only environments. Review tool use in your chain and enable shell only when you intend to grant command execution.

### Tool

**`local_shell`**

| Parameter | Type | Required | Description |
|---|---|---|---|
| `command` | string | ✅ | The executable alone — flags and operands go in `args`: `{"command": "ls", "args": ["-F"]}`, never `{"command": "ls -F"}` |
| `args` | string \| array | — | Everything after the executable: an array of argument strings, or a space-separated string |
| `cwd` | string | — | Working directory |
| `timeout` | string | — | Duration e.g. `30s` |
| `shell` | boolean | — | Run via `/bin/sh -c` (allows pipes, redirects, `$VAR`); without it the argv is executed directly and none of those are interpreted. **Disabled when `_allowed_commands` or `_allowed_dir` is set.** |

---

## Adding custom local tools

Adding new local tools types requires modifying the Contenox Go source code and implementing the `taskengine.HookRepo` interface. For custom capabilities without writing Go, build a small HTTP service (FastAPI, Express, etc.) and register it as a [Remote Tools](/docs/integrations/tools/remote) instead — no code changes required.
