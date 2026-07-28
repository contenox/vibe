---
description: Give a model controlled, policy-scoped access to the filesystem and shell on the machine contenox runs on.
---

# Local Tools

Local tools run on the same machine as Contenox. Most run directly inside the process; `local_shell` starts subprocesses when shell access is enabled. They are the fastest way to give a model controlled access to the machine it's running on.

## `local_fs` — Filesystem access

Always available. Provides read, write, search, and metadata operations scoped to a configured directory. **All paths are validated** against the allowed directory; attempts to escape with `../` are rejected.

The filesystem root is set when Contenox registers the local tool:

- `contenox run` / `contenox chat`: `--local-exec-allowed-dir <dir>` sets the root. Without a root, `local_fs` rejects file paths.
- ACP sessions use the editor/client workspace context where available.

`tools_policies.local_fs` controls read/output limits, denied path substrings, list filtering, and can override the root for a specific task with `_allowed_dir`.

### Tools

| Tool | Parameters | Description |
|---|---|---|
| `read_file` | `path` | Read the full content of a file. Also satisfies the read-before-mutate prerequisite for `write_file` / `edit_file` / `sed` against the same path. |
| `write_file` | `path`, `content` | Write content to a file (creates parent dirs, overwrites). For *existing* files, requires a prior full `read_file` against the same current file version in this session. |
| `edit_file` | `path`, `old_string`, `new_string`, `replace_all` (optional) | Replace an exact, byte-for-byte occurrence of `old_string` with `new_string` in an existing file — the targeted alternative to `write_file`'s full overwrite. See [`edit_file`](#edit_file-exact-string-replacement) below. |
| `list_dir` | `path` (optional) | List entries in a directory (dirs marked with `/`) |
| `read_file_range` | `path`, `start_line`, `end_line` | Read a specific line range. Satisfies targeted `edit_file` / `sed` mutations, but not full-file `write_file` overwrites. |
| `grep` | `path`, `pattern` | Search for a substring (or, with `regex: true`, a Go RE2 pattern). `path` may be a file **or a directory** — a directory search recurses through every text file beneath it. See [directory search](#grep-directory-search) below. |
| `find_files` | `pattern`, `path` (optional) | Find paths by glob pattern under the allowed root. `pattern` supports `**` to span any number of directories (e.g. `src/**/*.ts`); see [glob patterns](#find_files-globs) below. |
| `sed` | `path`, `pattern`, `replacement` | Replace a literal string in a file (not regex). For existing files, requires a prior `read_file` or `read_file_range` of the same path in this session. |
| `count_stats` | `path` | Count lines, words, and bytes (like `wc`) |
| `stat_file` | `path` | Get file metadata: name, size, mod time, isDir |

### `edit_file`: exact-string replacement

`edit_file` replaces one exact occurrence of `old_string` with `new_string` in an existing file, without resending the whole file — the preferred tool for a targeted change, cheaper and safer than `write_file`'s full overwrite.

- **Byte-exact and unique.** `old_string` must match the file's current on-disk text exactly, whitespace included. By default it must occur **exactly once**; if it matches zero times the file is left unchanged and the model is told to re-read and retry with the exact current text, and if it matches more than once the call is refused with a count so the model can add surrounding context to make it unique — a fuzzy or ambiguous match is never applied.
- **`replace_all`.** Set `replace_all: true` to replace every occurrence instead of requiring exactly one (e.g. renaming an identifier throughout the file).
- **Read-before-write.** Same contract as `sed`: a prior `read_file` or `read_file_range` of the current file version in this session is required before `edit_file` may run; the file's hash is re-verified immediately before writing, and a change since the read (by anyone) refuses the edit rather than clobbering it.
- Returns compact JSON (`path`, `written`, `replacements`, `old_bytes`, `new_bytes`, `old_sha256`, `new_sha256`) — not the full file bodies.

### `grep` directory search

`path` may name a file or a directory. Pointed at a directory, `grep` searches every text file beneath it recursively, applying the same `.gitignore` and high-noise-directory filtering as `list_dir` / `find_files`, and silently skipping binaries and unreadable files rather than aborting the search. Matches print as `relative/path:N: text` (a single-file search prints `N: text`). `start_line` / `end_line` apply only to a single-file search.

Directory search carries its own hard caps, tighter than a single-file search: matches stop at 100 regardless of `_max_grep_matches` (which is sized for one file), and the walk is bounded by the same `_max_find_depth` / `_max_list_scan` policy keys `find_files` and `list_dir` use. Hitting either cap returns the matches found so far with a notice to narrow the pattern or search a subdirectory.

### `find_files` globs

`find_files` uses Go's `filepath.Match` glob syntax (`*`, `?`, `[range]`) plus `**` to match zero or more whole path segments, crossing directory boundaries — e.g. `"src/**/*.ts"` finds every `.ts` file under `src` at any depth, including directly in `src` itself. A pattern with no slash matches against the file basename only (`"*.go"` finds Go files anywhere in the tree); a pattern with a slash (including one containing `**`) matches against the path relative to the search root.

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
| `_max_output_bytes` | int | `32768` (32 KiB) | Max byte size of any tool result returned to the model. Prevents listing a huge directory or grepping a large file from blowing up context. `0` or negative = unlimited. Prefer setting `_model_context_tokens` (below) over overriding this directly. |
| `_model_context_tokens` | int | unset | When set (and `_max_output_bytes` is not), derives the output cap as a fraction of the model's context window instead of using the fixed default. |
| `_max_list_depth` | int | `6` | Cap on `list_dir(recursive=true)`. Hard-capped at 32 regardless of policy. |
| `_max_list_scan` | int | `100000` | Cap on how many filesystem entries a single recursive `list_dir` visits, independent of how many it returns. |
| `_max_grep_matches` | int | `500` | Stops `grep` after this many matching lines (returned with a truncation notice) so the model narrows the pattern. Hard-capped at 500000. A directory search (`grep` pointed at a directory) is additionally capped at 100 matches regardless of this key. |
| `_max_find_results` | int | `200` | Caps `find_files` path results. Hard-capped at 5000. |
| `_max_find_depth` | int | `24` | Cap on how deep `find_files` descends, and (shared) how deep a directory `grep` search descends. Hard-capped at 128. |
| `_use_gitignore` | bool-string | `true` | Whether `.gitignore` is consulted when filtering `list_dir` and `find_files` output, in addition to `_skip_dir_names`. |
| `_skip_dir_names` | comma-sep | a built-in list of common noise directories (VCS metadata, `node_modules`, virtualenvs, build/dist/target output, editor directories, and more) | Directory basenames omitted by recursive `list_dir` and `find_files` when `.gitignore` doesn't already exclude them. Set to empty string to disable filtering and show everything. |
| `_list_extensions` | comma-sep | empty | Optional file extension filter for recursive `list_dir` output, e.g. `.go,.md,.json`. |
| `_denied_path_substrings` | comma-sep | empty | Path substrings that always reject (e.g. `node_modules,.git/,dist/`). Matched against the path relative to the allowed root. |
| `_verbose_tool_descriptions` | bool-string | `false` | Restore the long-form tool descriptions (truncation semantics, did-you-mean suggestions, the read-before-write contract) for large-context models. Off by default to save tokens on every turn. |

```json
"tools_policies": {
  "local_fs": {
    "_allowed_dir": ".",
    "_max_read_bytes": "1048576",
    "_max_output_bytes": "32768",
    "_max_list_depth": "6",
    "_max_grep_matches": "500",
    "_max_find_results": "200",
    "_skip_dir_names": ".git,node_modules,.venv,__pycache__,.next,dist,.cache,vendor,target,.idea,.vscode",
    "_denied_path_substrings": "node_modules,.git/,dist/,/.next/,/out/,package-lock.json"
  }
}
```

Values are strings even when conceptually numeric — `tools_policies` is the chain's policy carrier and uses string values uniformly across tools. The default chains (`default-chain.json`, `default-run-chain.json`) ship with conservative limit, root, and deny-substring defaults.

### Chain example

```json
"execute_config": {
  "model": "qwen2.5:7b",
  "provider": "ollama",
  "tools": ["local_fs"]
}
```

---

## `git` — Git operations

Available in `contenox chat`, `contenox run`, `contenox beam`, and ACP editor sessions (`contenox acp` / `acpx` — Zed, JetBrains, AionUi, OpenClaw). Runs in-process against the workspace's own Git repository (no `git` binary, no shell quoting) — the read tools are `allow` by default, the mutating ones require approval in the seeded HITL policies. The repository root is found by walking up from the allowed/working directory, same as `git` itself; network operations (push, pull, fetch, clone) are out of scope here — reach them through `local_shell` under its own policy.

### Tools

| Tool | Parameters | Description |
|---|---|---|
| `git_status` | — | Branch, HEAD commit, and what is staged, changed, or untracked. |
| `git_diff` | `path?` | Unified diff of the working tree against HEAD (staged and unstaged together). Optional `path` narrows it to one file or directory. |
| `git_log` | `n?`, `path?` | Recent commits, newest first: short hash, author, date, subject. `n` defaults to 10, capped at 200. |
| `git_show` | `ref` | One commit's metadata, message, and diff against its first parent. Accepts a hash, branch, tag, or `HEAD`/`HEAD~1`. |
| `git_branch_list` | — | Local branches with their head commits; the current branch is marked. |
| `git_blame` | `path` | Per-line authorship of a tracked file at HEAD: commit, author, line number, text. |
| `git_add` | `paths` | Stage a path (or array of paths) for the next commit. |
| `git_commit` | `message` | Commit what is staged. Refuses when nothing is staged; the author comes from the repository's own git config. |
| `git_checkout_branch` | `branch`, `create?` | Switch to a branch, or create it first with `create=true`. |
| `git_restore` | `paths`, `staged?` | **Destructive.** Discards uncommitted changes to the named paths, back to HEAD. With `staged=true` it only unstages them and leaves file contents alone. |

The default HITL presets (`hitl-policy-default.json`, `hitl-policy-acp.json`) allow `git_status`, `git_diff`, `git_log`, `git_show`, `git_branch_list`, and `git_blame`, and require approval for `git_add`, `git_commit`, `git_checkout_branch`, and `git_restore`.

### Chain example

```json
"execute_config": {
  "model": "qwen2.5:7b",
  "provider": "ollama",
  "tools": ["git"]
}
```

---

## `gointel` — Go code intelligence

Available in `contenox chat`, `contenox run`, `contenox beam`, and ACP editor sessions (`contenox acp` / `acpx` — Zed, JetBrains, AionUi, OpenClaw). Six read-only tools backed by a real Go type checker over the module containing the target directory (default: workspace root; host `GOOS`/`GOARCH`, no build tags, tests excluded). Prefer these over `grep`/`local_shell` for exact questions about Go symbols — they answer from the type graph, not from text search.

### Tools

| Tool | Parameters | Description |
|---|---|---|
| `go_describe` | `symbol`, `dir?` | Type, signature, doc comment, and (for named types) fields and methods of a Go symbol. |
| `go_definition` | `symbol`, `dir?` | Where a symbol is declared: `file:line:col` plus the source line. |
| `go_references` | `symbol`, `dir?` | Every use of a symbol in the module, resolved by type identity (not text), grouped by file. Capped at 50 results by default (max 200). |
| `go_implementations` | `symbol`, `dir?` | Both directions of the implements relation: types implementing an interface, and interfaces a type satisfies. |
| `go_symbols` | `dir?` | Outline of a package or a single `.go` file: every declaration, kind-tagged, with `file:line`. |
| `go_diagnostics` | `dir?` | Type/parse errors plus vet passes for a scope (`changed`, `package`, or `all`). Advisory — produced by this binary's own type checker, not the repository's toolchain; `go build` is the arbiter. |

`symbol` is qualified as `"pkg.Ident"`, `"pkg.Type.Method"`, or a bare `"Ident"`; an ambiguous name is refused with the qualified candidates listed. All six tools are `allow` by default in the seeded HITL policies.

### Chain example

```json
"execute_config": {
  "model": "qwen2.5:7b",
  "provider": "ollama",
  "tools": ["gointel"]
}
```

---

## `jq` — Structured data query

Available in `contenox chat`, `contenox run`, `contenox beam`, and ACP editor sessions (`contenox acp` / `acpx` — Zed, JetBrains, AionUi, OpenClaw). One tool, `jq_query`: runs a [jq](https://jqlang.org/) program (pure-Go `gojq`) over a JSON or YAML document and returns the emitted values. Read-only — a filter like `.a = 1` returns a modified copy, never touching the file on disk — with no network access and a bounded execution deadline (including recursion).

### Tool

**`jq_query`**

| Parameter | Type | Required | Description |
|---|---|---|---|
| `filter` | string | ✅ | The jq program, e.g. `.tasks[] \| select(.handler=="tools") \| .id`. |
| `path` | string | — | Document to query, relative to the workspace root. Mutually exclusive with `input`. |
| `input` | string | — | The document itself as a JSON or YAML string. Mutually exclusive with `path`. |
| `format` | string | — | `json` or `yaml`. Default: inferred from the file extension, then content. |
| `max` | integer | — | Maximum values to return (default 200, ceiling 5000). |
| `deadline_ms` | integer | — | Time budget in milliseconds (default 2000, ceiling 30000). |

Prefer `jq_query` over reading a whole config file when you only need one field or a projection — it costs a few tokens where the file costs thousands. `jq_query` is `allow` by default in the seeded HITL policies.

### Chain example

```json
"execute_config": {
  "model": "qwen2.5:7b",
  "provider": "ollama",
  "tools": ["jq"]
}
```

---

## `workspace` — Semantic search

Available in `contenox chat`, `contenox run`, `contenox beam`, and ACP editor sessions (`contenox acp` / `acpx` — Zed, JetBrains, AionUi, OpenClaw). One tool, `workspace_search`, over the semantic index built by [`contenox index`](/docs/reference/contenox-cli/). Returns ranked hits, each a `file:line-range` citation plus the matching text, so an answer can be attributed and the cited range re-read in full. A workspace with no index is not an error — the result names `contenox index` for the operator to run.

### Tool

**`workspace_search`**

| Parameter | Type | Required | Description |
|---|---|---|---|
| `question` | string | ✅ | Natural-language question about the workspace, phrased as what you want to find (the ranking is semantic, not keyword). |
| `top_k` | integer | — | Maximum citations to return. The result is also capped by an overall token budget and says how many hits it withheld. |

Results can go stale: a hit is flagged when the source file changed since the last index run. `workspace_search` answers by meaning and can be approximately right; for exact Go-symbol questions use the `gointel` tools instead. `workspace_search` is `allow` by default in the seeded HITL policies.

### Chain example

```json
"execute_config": {
  "model": "qwen2.5:7b",
  "provider": "ollama",
  "tools": ["workspace"]
}
```

---

## `goja` — JavaScript sandbox

Available in `contenox chat`, `contenox run`, `contenox beam`, and ACP editor sessions (`contenox acp` / `acpx` — Zed, JetBrains, AionUi, OpenClaw). `goja_eval` runs JavaScript (ES2023) in a sandbox with no network, no filesystem, no `require`/`import`, and no async — its only way out is `host.tool("provider.tool_name", {args})`, which calls another registered tool under the same HITL rules a direct model call would. The sandbox's result is the last expression evaluated.

Beyond `goja_eval`, Contenox scans `$CONTENOX_DIR/tools/*.js` at startup and registers one additional tool per script file, each under the name and description the script itself declares — a broken script fails at startup naming the file, rather than silently vanishing as a tool the operator believes still exists.

### Tool

**`goja_eval`**

| Parameter | Type | Required | Description |
|---|---|---|---|
| `code` | string | ✅ | The program to run. Its last expression is the result — end on the value you want, or an explicit `x` on its own line. |
| `deadline_ms` | integer | — | Time budget in milliseconds. |

`goja_eval` is `allow` by default in the seeded HITL policies. Operator-authored script tools carry no default rule, so they fall through to the active policy's `default_action`.

### Chain example

```json
"execute_config": {
  "model": "qwen2.5:7b",
  "provider": "ollama",
  "tools": ["goja"]
}
```

---

## `webtools` — HTTP calls

Always available. Lets the model call any HTTP endpoint via per-verb tools. Unlike remote tools (which require an OpenAPI spec), `webtools` exposes six generic verb tools — the model picks the verb, URL, query params, headers, and body at call time.

> [!CAUTION]
> Because the model controls the destination URL, every request is subject to size limits and an *opt-in* host policy configured via `tools_policies.webtools` (see below). By default `_denied_hosts` is empty — no host, including link-local / loopback / cloud-metadata addresses, is blocked unless you set it. Response size is capped at 1 MiB by default. Mutating verbs (`web_post`, `web_put`, `web_patch`, `web_delete`) trigger a HITL approval prompt by default. Do not point chains at untrusted user input without setting `_denied_hosts` (or an equivalent HITL policy rule with `op:"host"`) yourself.

### Tools

| Tool | Parameters | Description |
|---|---|---|
| `web_get` | `url`, `headers?`, `query?` | HTTP GET. Use for read-only retrieval. Default-allow under HITL. |
| `web_head` | `url`, `headers?`, `query?` | HTTP HEAD. Inspect headers / status without fetching the body. Default-allow under HITL. |
| `web_post` | `url`, `headers?`, `query?`, `body?` | HTTP POST. **HITL-approve by default.** |
| `web_put` | `url`, `headers?`, `query?`, `body?` | HTTP PUT. **HITL-approve by default.** |
| `web_patch` | `url`, `headers?`, `query?`, `body?` | HTTP PATCH. **HITL-approve by default.** |
| `web_delete` | `url`, `headers?`, `query?`, `body?` | HTTP DELETE. **HITL-approve by default.** |

**Parameter shapes:**

| Parameter | Type | Description |
|---|---|---|
| `url` | string | Absolute URL. Scheme must be in `_allowed_schemes` (default `http,https`). Host is checked against `_allowed_hosts` / `_denied_hosts`. |
| `headers` | object \| string | JSON object `{"X-Foo":"bar"}` (preferred). A JSON-encoded string is also accepted for back-compat. |
| `query` | string | URL-encoded query string (e.g. `a=1&b=2`). Merged with the URL's existing query. |
| `body` | any | Used by mutating verbs only. Strings sent as-is; any other JSON value is marshalled. Capped by `_max_request_body_bytes` (default 256 KiB). |

### `tools_policies.webtools` keys

| Key | Type | Default | Description |
|---|---|---|---|
| `_allowed_hosts` | comma-sep | empty (any host) | When set, *only* listed hosts pass. |
| `_denied_hosts` | comma-sep | empty (no host denied) | Opt-in SSRF guard — set this yourself to block link-local / loopback / cloud-metadata hosts, e.g. `169.254.169.254,169.254.170.2,localhost,127.0.0.1,0.0.0.0,::1,metadata.google.internal,metadata.azure.com`. Only the `hitl-policy-acpx.json` HITL preset denies these hosts by default (via an `op:"host"` rule); `webtools` itself does not. |
| `_allowed_schemes` | comma-sep | `http,https` | Block `file://`, `gopher://`, `ftp://`, etc. |
| `_max_response_bytes` | int | `1048576` (1 MiB) | `0` or negative = unlimited. Truncated responses include a marker. |
| `_max_request_body_bytes` | int | `262144` (256 KiB) | `0` or negative = unlimited. Oversized body blocks the call before sending. |
| `_request_timeout_seconds` | int | `30` | Per-call timeout. |
| `_max_attempts` | int | `3` | Retries 5xx and transport errors only — never 4xx. |
| `_initial_backoff_ms` | int | `250` | Exponential backoff with jitter. |
| `_max_backoff_ms` | int | `5000` | Cap on the exponential backoff. |
| `_disallow_redirects` | bool-string | `"false"` | When `"true"`, blocks all 3xx redirect-following. |

### Chain example

```json
"execute_config": {
  "model": "qwen2.5:7b",
  "provider": "ollama",
  "tools": ["webtools"],
  "tools_policies": {
    "webtools": {
      "_allowed_hosts": "api.github.com,api.openai.com",
      "_max_response_bytes": "524288",
      "_request_timeout_seconds": "20"
    }
  }
}
```

---

## `local_shell` — Shell command execution

> [!CAUTION]
> `local_shell` gives the model direct access to run arbitrary commands on your machine. **Never enable it in public-facing deployments or when processing untrusted user input.**

For direct CLI use, `local_shell` is opt-in. Enable it per invocation with `--shell`:

```bash
contenox run --shell "clean up unused imports in the codebase"
contenox chat --shell "run the tests and fix anything that breaks"
```

**Command policy is set in the chain, not on the CLI.** Add a `tools_policies` block to `execute_config`:

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
- `_allowed_dir` — if set, the command executable or script path must reside under this directory. The global `--local-exec-allowed-dir` flag sets the same executable/script boundary for an invocation.

The default chains (`default-chain.json`, `default-run-chain.json`) ship with sensible defaults: common dev tools allowed, privilege-escalation and raw-disk commands denied.

To use `local_shell` with **no policy restrictions** (fully open), omit `tools_policies` entirely. Only do this in fully trusted, local-only environments. Review tool use in your chain and enable shell only when you intend to grant command execution.

### Tool

**`local_shell`**

| Parameter | Type | Required | Description |
|---|---|---|---|
| `command` | string | ✅ | Executable path or name |
| `args` | string \| array | — | Space-separated arguments string, or an array of argument strings |
| `cwd` | string | — | Working directory |
| `timeout` | string | — | Duration e.g. `30s` |
| `shell` | boolean | — | Run via `/bin/sh -c` (allows pipes, redirects, `$VAR`). **Disabled when `_allowed_commands` or `_allowed_dir` is set.** |

---

## `print` — Append to conversation

Always available. Appends a message to the chat history as a system message, or returns the message as a plain string when no chat history is active.

### Tool

**`print`**

| Parameter | Type | Required | Description |
|---|---|---|---|
| `message` | string | ✅ | Text to append or return |

### Chain example

```json
"execute_config": {
  "model": "qwen2.5:7b",
  "provider": "ollama",
  "tools": ["print"]
}
```

---

## `echo` — Debug passthrough

Always available. Echoes the input back, prefixed with `"Echo: "`. Useful for verifying what a task receives during chain development.

### Tool

**`echo`**

| Parameter | Type | Required | Description |
|---|---|---|---|
| `input` | string | ✅ | Text to echo |

### Chain example

```json
"execute_config": {
  "model": "qwen2.5:7b",
  "provider": "ollama",
  "tools": ["echo"]
}
```

---

## Adding custom local tools

Adding new local tools types requires modifying the Contenox Go source code and implementing the `taskengine.HookRepo` interface. For custom capabilities without writing Go, build a small HTTP service (FastAPI, Express, etc.) and register it as a [Remote Tools](/docs/integrations/tools/remote) instead — no code changes required.
