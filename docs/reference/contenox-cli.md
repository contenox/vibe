---
title: Contenox CLI Reference
description: Every contenox subcommand, flag, and environment variable.
order: 3
---

# Contenox CLI Reference

Every contenox subcommand, flag, and environment variable. Agents, tools, models and the rules they run under are files on your machine, and so is everything it executes.

Three commands are the ones you reach for; the rest configure what they run. [`contenox beam`](#contenox-beam) is the front door — you, at a terminal. [`contenox run`](#contenox-run) is the scripting shape — a program is the caller. [`contenox serve`](#contenox-serve-path) is the standing host — an organization is accountable for it.

## Global Flags

Persistent flags on the root command (also shown under **Global Flags** on subcommands). Run `contenox --help` for the full list.

| Flag                             | Description                                                                                                                       |
| -------------------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| `--model <name>`                 | Model override for this invocation; persistent default is `contenox config set default-model <name>`                               |
| `--provider <type>`              | Provider override for this invocation. See `contenox backend add --help` for supported backend types. |
| `--db <path>`                    | SQLite DB path (default: `~/.contenox/local.db`). The one global database is shared by every workspace. |
| `--data-dir <path>`              | Override the `.contenox` data directory (skips walk-up search). Used to locate the workspace's `workspace.id` and chain files; does not change the database location. |
| `--timeout`                      | Max execution time per invocation (default `2h`)                                                                                  |
| `--context`                      | Context length hint for the tokenizer                                                                                             |
| `--ollama`                       | Ollama base URL (default `http://127.0.0.1:11434`)                                                                                |
| `--no-delete-models`             | Legacy compatibility flag; a no-op in the OSS runtime (model deletion is disabled). Defaults to **true**.                          |
| `--trace`                        | Structured operation telemetry on stderr                                                                                          |
| `--steps`                        | Print execution steps after the result                                                                                            |
| `--think <level>`                | Set reasoning level for supported models: `auto`, `off`, `minimal`, `low`, `medium`, `high`, `xhigh`                              |
| `--alt-model <name>`             | Alt model name (for chains referencing `{{var:alt_model}}`). Overrides `default-alt-model` config.                                  |
| `--alt-provider <type>`          | Alt provider type (for chains referencing `{{var:alt_provider}}`). Overrides `default-alt-provider` config.                          |
| `--max-tokens <N>`               | Response token cap (for chains referencing `{{var:max_tokens}}`). Overrides `default-max-tokens` config.                             |

## Subcommands

### `contenox setup`

Runs an interactive setup wizard to configure your primary provider, model, and API key. This is the recommended first step for all new users. It ensures your global `~/.contenox/` configuration is ready for use.

```bash
contenox setup
```

The wizard guides you through picking a provider (local Ollama, Ollama Cloud, OpenAI, Anthropic, Google Gemini, Vertex AI, AWS Bedrock, or self-hosted vLLM), entering an API key or base URL where needed, and setting your first default model. It needs a real terminal (reads answers from stdin) and will not guess a default from a closed or piped stdin.

### `contenox beam`

The first-party terminal client, and the front door: a person at the keyboard, on their own device. Bare `contenox` on a TTY opens it, so `contenox` and `contenox beam` are the same command.

```bash
contenox                     # on a terminal, this is beam
contenox beam                # the same, said explicitly
contenox beam --new          # start a fresh session instead of resuming
contenox beam --session api-review   # open a named session
```

The transcript is written into your native scrollback rather than a managed pane, so it scrolls, copies and searches like everything else in that window. The composer takes `/` for commands and `@` to put a file in front of the agent. A gated tool call raises an **approval card** inline — the tool, its arguments, and the rule that gated it — answered with one keystroke; leave it unanswered and the turn checkpoints into a [durable ask](#contenox-approvals) instead of holding the process open.

`local_fs` and `local_shell` work natively here: beam is the ACP client, so it performs the filesystem and terminal calls itself, in the workspace the instance was launched with.

| Flag | Description |
| ---- | ----------- |
| `--new` | Start a new session rather than resuming the active one |
| `--session <name>` | Open (or create) the named session |
| `--light` | Light-background colour scheme |
| `--plain` | Plain output: no colour and no redrawing, for terminals and captures that want neither |
| `--log-dir <dir>` | Write structured logs here (default: `<data-dir>/logs`) |

### `contenox run`

The scripting shape: a program is the caller — CI, a cron entry, a Makefile, another agent. It runs one task with the tools on that machine, prints the report to stdout, and exits.

```bash
contenox run "summarise what changed under ./internal since Friday"
contenox run reviewer "review the payment retry change"
```

The first argument is a declared agent when it names one, and part of the task otherwise; with no agent named, the preseeded `run` declaration handles it. Exit status is 0 when the work landed and nonzero when it did not, so a pipeline can branch on it without parsing the report.

There is no terminal in front of `run`, so a gated call has nobody to ask: it becomes a durable ask like any other, answerable later with [`contenox approvals respond`](#contenox-approvals), and the run resumes from its checkpoint exactly once. Bound what a scripted run may do unattended in its envelope rather than by watching it.

<h3 id="sessions"><code>contenox session</code></h3>

Manage named chat sessions. Each session maintains its own conversation history. `list` and `show` default to the active scope; the whole database can also be inspected across workspaces and namespaces, and any session opened directly by id — useful for recovering a session an editor lost track of.

```bash
contenox session list                    # list all sessions (* = active)
contenox session new [name]             # create a session (becomes active)
contenox session switch <name>          # switch to a different session
contenox session show                   # show active session's history
contenox session show <name>            # show any session by name
contenox session show <id>              # show any session by id (any workspace)
contenox session show <id> --ns <name>  # namespace hint when resolving by id (advisory)
contenox session show --tail 10         # show last 10 messages
contenox session show --head 5          # show first 5 messages
contenox session show default --tail 6  # tail a non-active session
contenox session fork [name]            # copy the active session to a new one (becomes active)
contenox session fork --summary         # compact older history into a summary, then fork and continue
contenox session fork --summary --keep 12  # keep the last 12 messages verbatim (default: 8)
contenox session delete <name>          # delete session and all messages
```

`session fork` branches the current conversation into a new session so you can explore an alternate direction without losing the original. `--summary` first compacts the older turns into a summary (via `chain-compact-default.json`) before forking, which trims a long history while preserving context; `--keep` sets how many of the most recent messages stay verbatim instead of being summarized.

Inspect the whole database, not just the active workspace/identity:

```bash
contenox session workspaces              # list workspaces and namespaces (counts)
contenox session list --all              # every session across the whole DB
contenox session list --workspace <id>   # sessions in a workspace
contenox session list --namespace <ns>   # sessions in a namespace (e.g. jetbrainsgoland)
```

A namespace is the session-name prefix before its generated id (e.g. `jetbrainsgoland`, `zed`, `default`). To recover a session an editor abandoned: find it with `session list --namespace <ns>`, then `session show <id>`.

### Workspace authority

**One instance serves exactly one workspace, fixed at launch.** There is nothing to select afterwards, and no client anywhere offers a picker.

Which directory that is depends on the shape:

| Shape | The workspace is |
| --- | --- |
| [`contenox beam`](#contenox-beam), [`contenox run`](#contenox-run) | the directory you started it in |
| [`contenox serve [path]`](#contenox-serve-path) | the path you named, or your home directory when you name none |
| [`contenox acp`](#contenox-acp--contenox-acpx) | whatever the editor opened — per the protocol, the client owns the cwd |

A client does not propose a working directory and the runtime does not offer a menu of them. An app or an editor **discovers** instances and the sessions they are already holding, and attaches to one; the workspace is a property of the instance it attached to. Serving a second workspace means starting a second instance — which is also how the process list says so.

The runtime's own control plane (`~/.contenox` and a workspace's `.contenox` — its config, database, and policies) is never a workspace, on any shape. There is no setting that turns this off.

A mission dispatched from an instance inherits that instance's workspace: a unit cannot be given a working directory the session that fired it could not have opened.

### `contenox doctor`

Prints local LLM setup readiness: default model, default provider, and backend reachability.

```bash
contenox doctor
contenox doctor --json          # machine-readable output
contenox doctor --skip-cycle    # faster; skips backend sync (status may be stale)
contenox doctor --bundle        # also write a redacted diagnostics zip to attach to an issue
```

| Flag            | Description                                              |
| --------------- | ---------------------------------------------------------- |
| `--json`        | Print results as JSON instead of human-readable text     |
| `--skip-cycle`  | Skip syncing backends before the check (faster but may show stale status) |
| `--bundle`      | Also write a redacted diagnostics zip and print a pre-filled GitHub issue URL |
| `--bundle-out <path>` | Where `--bundle` writes (default: `./contenox-doctor-<timestamp>.zip`) |

`--bundle` is the flag to reach for when you are stuck and filing a bug. The archive holds `doctor.json` (this report), `build.txt` (version, Go toolchain, platform, VCS build settings), and the tail of every `telemetry.log` it finds — capped at the last 256 KB each, one entry per source directory (workspace `.contenox`, the database's directory, `~/.contenox`). Every member is passed through credential redaction on the way in: named assignments (`api_key`, `token`, `password`, `authorization`, …), URL userinfo, `Bearer` values, and recognizable provider key shapes are replaced with `[REDACTED]`, and the command prints how many values it replaced. The issue URL is pre-filled with the environment facts a maintainer asks for first and names the bundle as the attachment — it carries no log content, so nothing leaves your machine until you attach the file yourself. Review the zip before sharing it.

`doctor` also reports vision-capable model availability, flags a HITL policy preset that predates the currently shipped toolset (fix with `contenox init --refresh-policies`), and warns — without changing anything — when `default-max-tokens` exceeds the active provider's output-token ceiling.

When one of the [external state backends](/docs/reference/config/#external-backends-for-state-opt-in) is selected, `doctor` adds a **State storage** section naming the backend behind the store, the message bus and the key-value cache, and whether a remote one answered; an unreachable one is named with the variable that selected it. With none of them set the section is absent and the report is what it always was.

### `contenox model`

Inspect models from configured LLM backends and manage capability overrides. Managing the local **model registry** (adding custom entries with a URL) is not part of the current CLI — `model list`, `model set-context`, and `model capability` are the full subcommand tree.

#### `contenox model list`

Query every registered backend in real time and show models that can be used now, with observed capabilities (chat, embed, prompt, think, vision) and context length.

```bash
contenox model list
```

#### `contenox model set-context`

Override the locally stored context window for a model the runtime already knows about (one that has appeared in `model list`). Useful when a backend reports a different (or no) context size than the model actually supports.

```bash
contenox model set-context qwen3:8b             --context 32k
contenox model set-context gpt-5-mini           --context 128k
contenox model set-context gemini-flash-latest  --context 1m
```

| Flag        | Description                                                      |
| ----------- | ------------------------------------------------------------------ |
| `--context` | Context window size: bare integer or shorthand (`12k`, `128k`, `1m`); `0` clears the override. Required. |

#### `contenox model capability`

Manage manual provider/model capability overrides — the reasoning (`think`) and image-input (`vision`) capabilities the runtime assumes for a given provider/model when the catalog doesn't declare them.

```bash
contenox model capability set   <provider> <model> --think true   # mark the model as supporting reasoning
contenox model capability set   <provider> <model> --vision true  # mark the model as accepting image input
contenox model capability show  <provider> <model>                # show the current override
contenox model capability unset <provider> <model>                # remove the override (revert to catalog)
```

`capability set` requires at least one of `--think` or `--vision` (each `true`/`false`).

### `contenox tools`

Manage remote OpenAPI tools. See [Remote Tools](/docs/integrations/tools/remote) and [Tools Allowlist Patterns](/docs/integrations/tools/#how-it-works).

```bash
contenox tools add <name> --url <url>
contenox tools add <name> --url <url> --header "Authorization: Bearer $TOKEN" --inject "tenant_id=acme"
contenox tools add <name> --url <url> --spec ~/my-spec.yaml   # local file spec
contenox tools list
contenox tools show <name>
contenox tools update <name> --header <...> --inject <...> --spec <url-or-path>
contenox tools remove <name>
```

| Flag        | Description                                                                                |
| ----------- | ------------------------------------------------------------------------------------------ |
| `--url`     | Base URL of the service — where API calls are sent (required)                              |
| `--spec`    | URL or local file path of the OpenAPI v3 spec (`https://...`, `~/path`, `./path`, `/abs/path`). Local paths stored as `file://` URIs; must exist at registration time. Defaults to `<url>/openapi.json`. |
| `--header`  | HTTP header to inject on every call, e.g. `"Authorization: Bearer $TOKEN"` (repeatable)    |
| `--inject`  | Tool call argument to inject and hide from the model, e.g. `"tenant_id=acme"` (repeatable) |
| `--timeout` | Request timeout in milliseconds (default: 10000)                                           |
| `--insecure-skip-tls-verify` | Skip TLS certificate verification for this provider (add-time only; self-signed/internal services) |

For an API that needs a login step before each session (session-cookie or token-based auth, e.g. Frappe/ERPNext or a legacy service with no API-key support), register the login flow at `add` time — Contenox performs the login automatically on 401/403 and retries:

| Flag                       | Description                                                                     |
| -------------------------- | -------------------------------------------------------------------------------- |
| `--auth-login-url`         | URL to POST credentials to (setting this enables the login flow)                |
| `--auth-login-method`      | HTTP method for the login request (default `POST`)                              |
| `--auth-login-body`        | JSON body for the login request; `${ENV_VAR}` placeholders expand at runtime    |
| `--auth-extract-cookie`    | Name of a `Set-Cookie` cookie to extract from the login response                 |
| `--auth-extract-jsonpath`  | JSONPath expression to extract a token from the login response body             |
| `--auth-inject-header`     | HTTP header to carry the extracted value on API calls                           |
| `--auth-inject-format`     | `printf` format for the injected value, e.g. `"Bearer %s"` (defaults to a cookie `name=value` pair when extracting a cookie) |

```bash
# Frappe/ERPNext — session cookie login
contenox tools add erp --url https://erp.local \
  --insecure-skip-tls-verify \
  --auth-login-url https://erp.local/api/method/login \
  --auth-login-body '{"usr":"${FRAPPE_USER}","pwd":"${FRAPPE_PASS}"}' \
  --auth-extract-cookie sid \
  --auth-inject-header Cookie
```

The login-flow flags and `--insecure-skip-tls-verify` can only be set at `tools add` time; to change them, remove the provider and re-add it.

### `contenox agent`

> **Beta:** the agent roster requires `contenox config set opt-in-beta true` (or `CONTENOX_OPT_IN_BETA=1`) and its interface may change; without it this command is hidden and only the shipped `agent-planner` is discovered (`agent-planner` is the chain's `id`, declared inside `chain-planner-default.json` — see [Chain files: naming, roles, and resolution](/docs/guide/chains/naming/)).

Inspect and manage the runtime's declared agents. Most agents are [declared in a Markdown file](/docs/guide/agents/) under `.contenox/agents/`; agents you already keep in `.claude/agents/` or `.agents/agents/` are found there too, and a task chain on disk is an agent as well. Every one is registered automatically by discovery — this command inspects them, toggles their enabled state, and removes stale registrations. Declared agents are what `/mission` and `contenox mission fire` dispatch.

```bash
contenox agent list                       # id, name, source, kind, enabled
contenox agent show agent-reviewer        # provenance + config_json
contenox agent enable agent-reviewer      # (and: disable)
contenox agent remove agent-reviewer      # (alias: rm)
```

`remove` deletes only the local registration; discovery may re-register it on the next startup if its chain file still exists.

### `contenox vet [path]`

Validate chain files and HITL policy (envelope) files before anything runs them.

```bash
contenox vet                  # every .json in the workspace .contenox/
contenox vet --all            # the workspace .contenox/ plus ~/.contenox/
contenox vet chain.json       # one file
contenox vet ./mychains/      # every .json under a directory
```

| Flag    | Description                                                            |
| ------- | -------------------------------------------------------------------------- |
| `--all` | Vet the workspace `.contenox/` and the global `~/.contenox/` directory |

Files are classified by content: a `"tasks"` array is a chain, a `"rules"` array (or a `hitl-policy-*.json` name) is an envelope; anything else is skipped. A chain is checked with the load-time linter (handler input/output signatures, dataflow across every `goto`/`on_failure` edge, `input_var` and template references, branches that can never fire, structural defects). A policy is checked for unknown fields, invalid rule shapes, tool patterns that can never match, and timeout values. A file can also print a `WARN` line — a field that parses and is accepted but is not enforced as strongly as it reads; warnings never fail the run. `vet` exits non-zero when any vetted file fails.

### `contenox hitl trust [command-or-path ...]`

Declare, refresh, or list the binaries a policy's allow rules may run. An allowlist entry pins a command **name**; `PATH` decides what that name is. This records the absolute real path a name resolves to and that file's SHA256 into the policy's `trusted_binaries` block, so a substituted or tampered binary is refused instead of inheriting the allow.

```bash
contenox hitl trust go git          # declare two binaries by name
contenox hitl trust /usr/bin/make   # declare one by absolute path
contenox hitl trust --refresh       # re-read every declaration (upgrade path)
contenox hitl trust --list          # show every declaration's state on this host
contenox hitl trust --remove go     # drop a declaration
```

| Flag | Description |
| ---- | ----------- |
| `--policy <name\|path>` | Policy to update: a preset name resolved along the policy search path, or an explicit file path (default `hitl-policy-default.json`) |
| `--refresh` | Re-read every already-declared binary and rewrite its hash — the legitimate-upgrade path |
| `--list` | List every declaration and its state on this host; changes nothing |
| `--remove` | Remove the named declarations instead of adding them |

Names are resolved exactly as the policy evaluator resolves them (`PATH` lookup, `PATHEXT` on Windows, then symlinks followed to the real file), so a declaration written here is by construction the one the evaluator will look up. Declarations are spliced into the policy without disturbing any other byte of the file, and the result is validated before it is written. Declaring any hash makes the pin strict for that policy: a command with no declared hash is refused. See [Trusted binaries](/docs/guide/confinement/trusted-binaries/) for the full workflow, the per-platform guarantees, and what this does not protect.

### `contenox init [provider]`

Initializes a workspace (`.contenox/`) and ensures default runtime presets exist globally (`~/.contenox/`). It's best to run `contenox setup` first for a guided configuration.

`init` creates the `.contenox/workspace.id` marker — a project's portable identity. The marker carries a stable workspace UUID (the database scoping token every session under the project is filed under) plus an optional friendly **name**. It travels *with* the directory, so a project means one thing to the CLI and every ACP session alike. It also seeds `agents.toml` and an `agents/` directory — where you [declare an agent](/docs/guide/agents/) — plus the HITL policies and the [oracle](/docs/use-cases/auto-attention/) set (`chain-oracle-default.json` and `hitl-policy-oracle.json` — inert until `default-oracle-chain` names one) under `~/.contenox/`, and the shipped chain files under `~/.contenox/system/`, unless they already exist. Workspace-local `.contenox/` files can override these global presets by name; `init --local` seeds those workspace copies for you instead of writing to `~/.contenox/`. The seeded chain files follow the `chain-<role>-<variant>.json` convention — [Chain files: naming, roles, and resolution](/docs/guide/chains/naming/) covers the grammar and the exact touch/never-touch matrix of every init flag.

By default `init` walks up to reuse an ancestor's `.contenox` if one exists (like `git`). Pass `--project` to force a *fresh* project marker in the current directory instead — a distinct workspace nested under a larger one — and `--name` to give it a friendly name (default: the folder's own name).

You can optionally specify a provider to pre-configure defaults.

```bash
contenox init                          # scaffold a workspace
contenox init gemini                   # pre-configure for Gemini
contenox init openai                   # pre-configure for OpenAI
contenox init --force                  # overwrite existing files
contenox init --update                 # refresh unchanged default files
contenox init --refresh-policies       # rewrite only the HITL policy presets
contenox init --local                  # seed workspace-local override copies
contenox init --project --name "API"   # a fresh named project in the current dir
```

| Flag        | Description                         |
| ----------- | ----------------------------------- |
| `-f, --force` | Overwrite existing preset files |
| `--update`  | Refresh unchanged default files to the latest embedded versions; first renames shipped chain files still under a pre-v0.38 name (e.g. `default-acp-chain.json`) to the `chain-<role>-<variant>.json` convention, byte-for-byte, in `~/.contenox` and the workspace `.contenox` both |
| `--local`   | Write the chain files and HITL policy presets into the workspace `.contenox/` instead of `~/.contenox` — deliberate workspace-local overrides that win over the global copies by name |
| `--refresh-policies` | Rewrite only the HITL policy presets (`hitl-policy-*.json`) in `~/.contenox` from this build; chains, config, and sessions are left untouched — this is what `contenox doctor` points at when an envelope predates a shipped toolset |
| `--project` | Create a fresh project marker in the current directory (a new workspace id) instead of reusing an ancestor's `.contenox` |
| `--name <name>` | Friendly project name for the marker (default: the directory name) |

### `contenox backend`

Register and manage LLM backend endpoints.

```bash
contenox backend add ollama       --type ollama
contenox backend add ollama-cloud --type ollama --url https://ollama.com/api --api-key-env OLLAMA_API_KEY
contenox backend add openai       --type openai  --api-key-env OPENAI_API_KEY
contenox backend add anthropic    --type anthropic --api-key-env ANTHROPIC_API_KEY
contenox backend add bedrock      --type bedrock --url https://bedrock-runtime.us-east-1.amazonaws.com
contenox backend add gemini       --type gemini  --api-key-env GEMINI_API_KEY
contenox backend add myvllm       --type vllm    --url http://gpu-host:8000
contenox backend add vertex       --type vertex-google \
  --url "https://us-central1-aiplatform.googleapis.com/v1/projects/YOUR_PROJECT_ID/locations/us-central1"

contenox backend list
contenox backend show openai
contenox backend remove myvllm
```

| Flag            | Description                                                                               |
| --------------- | ----------------------------------------------------------------------------------------- |
| `--type`        | Backend type (default `ollama`). Not validated against a fixed enum — see below. |
| `--url`         | Base URL. Inferred automatically for `ollama`, `openai`, `anthropic`, and `gemini` when omitted; **required** for `vllm`, `bedrock`, and `vertex-google` (`bedrock`/`vertex-google` error immediately if omitted, since their URL is account-specific and cannot be defaulted) |
| `--api-key-env` | Environment variable holding the API key (preferred)                                      |
| `--api-key`     | API key literal (avoid — use `--api-key-env`)                                             |

`--type` accepts any string; only `ollama`, `openai`, `anthropic`, and `gemini` get an inferred base URL. Pass `--url` explicitly for `vllm`, `bedrock`, `vertex-google`, or any other type.

### `contenox config`

Manage persistent CLI defaults stored in SQLite.

```bash
contenox config set default-provider ollama
contenox config set default-model    qwen3:8b
contenox config set default-alt-model gemini-3.6-flash
contenox config set default-alt-provider gemini
contenox config set default-autocomplete-model qwen2.5-coder:7b
contenox config set default-autocomplete-provider ollama
contenox config set default-audio-model gemini-2.5-flash
contenox config set default-audio-provider gemini
contenox config set default-max-tokens 8192
contenox config set default-think high
contenox config set default-chain    .contenox/my-chain.json
contenox config set hitl-policy-name hitl-policy-strict.json

contenox config get default-model
contenox config list
```

Valid global keys: `default-model`, `default-provider`, `default-alt-model`, `default-alt-provider`, `default-autocomplete-model`, `default-autocomplete-provider`, `default-audio-model`, `default-audio-provider`, `default-max-tokens`, `default-think`, `telemetry-enabled`, `update-check`, `opt-in-beta`, `default-mission-agent`, `default-mission-policy`, `default-oracle-chain`, `default-oracle-policy`, `oracle-approves-tool-calls`, `fleet-max-parallel`. `opt-in-beta` (`true`/`false`) enables the beta features — the agent roster and the [event tier](/docs/guide/events/) — which are otherwise absent entirely.

Valid workspace keys: `default-chain`, `hitl-policy-name`.

| Key | Description |
|---|---|
| `default-audio-model` | Model preferred for requests carrying audio attachments, independent from `default-model`. Unset falls back to `default-model`; audio requests resolve only to audio-capable models either way. |
| `default-audio-provider` | Provider type for the audio model, independent from `default-provider`. Unset uses `default-provider`. |
| `default-mission-agent` | Declared agent the ACP `/mission <intent>` slash command falls back to when none is named. `contenox mission fire` always requires the agent name as a positional argument, so this key does not affect it. |
| `default-mission-policy` | Envelope (HITL policy) name that both `/mission` and `contenox mission fire --policy` fall back to when none is named. `/mission --policy <envelope>` overrides it for one mission. It is also the envelope a subagent started by `/plan` or the `mission_start` tool runs under. |
| `default-oracle-chain` | Chain that adjudicates a subagent's asks, e.g. `chain-oracle-default.json`. **Setting it is what turns the [oracle](/docs/use-cases/auto-attention/) on**; unset means no oracle and every ask waits for a human. `contenox acp --oracle <chain>` overrides it for one run, and `--oracle off` disables it. |
| `default-oracle-policy` | Envelope the oracle chain itself runs under. Unset uses `hitl-policy-oracle.json`. Override per run with `--oracle-policy`. |
| `oracle-approves-tool-calls` | `true`/`false` (default false). Lets the oracle rule on a subagent's `approve`-tier **tool calls**, not just its questions. The subagent's own envelope must also grant `attention.allowAgentApprovals` — both have to agree. Override per run with `--oracle-approves-tool-calls`. |
| `fleet-max-parallel` | Fleet-wide admission cap: max concurrently open mission units (integer; `0` = unlimited; default 8). |

`contenox config list` shows each key's current value **and its scope** (`global` / `workspace`) so you can see whether a setting is inherited or overridden locally.

The `default-*` model settings can also be overridden per process — without persisting anything — via the `CONTENOX_DEFAULT_*` environment variables; see the [environment variables table](#environment-variables) below.

### `contenox mcp`

Register and manage MCP (Model Context Protocol) servers.

```bash
# Shorthand: name + URL (transport defaults to http)
contenox mcp add notion https://mcp.notion.com/mcp --auth-type oauth

# Stdio transport (local process)
contenox mcp add myserver --transport stdio --command npx \
  --args "-y,@modelcontextprotocol/server-filesystem,/tmp"

# SSE transport (remote) with bearer auth
contenox mcp add remote --transport sse --url https://mcp.example.com/sse \
  --auth-type bearer --auth-env MCP_TOKEN

# Inject hidden params into every tool call (model never sees them)
contenox mcp add myserver --transport http --url http://localhost:8090 \
  --header "X-Tenant: acme" \
  --inject "tenant_id=acme" --inject "env=production"

# OAuth with pre-issued client credentials (HubSpot, Salesforce, MS Graph,
# any vendor MCP without RFC 7591 dynamic registration)
contenox mcp add hubspot --transport http --url https://mcp.hubspot.com/ \
  --auth-type oauth \
  --oauth-client-id <client_id from vendor UI> \
  --oauth-client-secret-env HUBSPOT_MCP_CLIENT_SECRET

# For OAuth servers, run the authorization flow AFTER adding (opens a browser).
# This is a required, separate step — `mcp add --auth-type oauth` only registers
# the server; it does not authenticate it. Re-run only when the token expires.
contenox mcp auth notion

contenox mcp list
contenox mcp show myserver
contenox mcp update myserver --inject "tenant_id=newvalue"
contenox mcp remove myserver
```

For OAuth servers the full sequence is: `contenox mcp add <name> ... --auth-type oauth`, then `contenox mcp auth <name>` to complete the OAuth 2.1 PKCE flow in the browser. The token is stored locally and reused until it expires.

| Flag           | Description                                                                                |
| -------------- | ------------------------------------------------------------------------------------------ |
| `[url]`        | URL as a second positional arg — sets `--url` and defaults `--transport` to `http`         |
| `--transport`  | Server transport: `stdio`, `sse`, `http`                                                   |
| `--command`    | Command to execute (stdio only)                                                            |
| `--args`       | Comma-separated command arguments                                                          |
| `--url`        | Remote endpoint URL (sse, http)                                                            |
| `--auth-type`                | Authentication type: `bearer` or `oauth`                                                         |
| `--auth-env`                 | Environment variable holding auth token (preferred over `--auth-token`)                          |
| `--auth-token`               | Auth token literal (avoid — use `--auth-env`)                                                    |
| `--oauth-client-id`          | Pre-issued OAuth `client_id` for vendors without RFC 7591 dynamic registration (HubSpot, etc.)   |
| `--oauth-client-secret-env`  | Env var holding the pre-issued OAuth `client_secret` (only the var name is stored locally)       |
| `--header`                   | Additional HTTP header for SSE/HTTP connections, e.g. `"X-Tenant: acme"` (repeatable)            |
| `--inject`                   | Tool call argument to inject and hide from the model, e.g. `"tenant_id=acme"` (repeatable)       |
| `--timeout`                  | Connection timeout in seconds (0 = no timeout)                                                   |

> **Note:**
> `mcp update --header` and `mcp update --inject` each **replace** the entire corresponding map. Pass all required values in a single update call. `mcp update` cannot change `--transport`, `--command`, `--args`, or `--url` — remove and re-add the server for those.

### `contenox mission`

Fire and inspect missions: unattended work orders dispatched at a declared agent, run inside an envelope (a named HITL policy that bounds what the unit may do unattended), with durable reports.

```bash
contenox mission list                       # newest first: id, agent, envelope, status, age
contenox mission show <mission-id>          # record, plan summary, and report summaries
contenox mission reports <mission-id>       # every report in full detail
contenox mission plan <mission-id>          # the living plan: entries, status, revision history
contenox mission asks [mission-id]          # pending questions (one mission, or every open mission)
contenox mission fire agent-reviewer "review the open PR for regressions" --wait
contenox mission stop <mission-id> --reason "no longer needed"
```

| Flag (subcommand)             | Description                                                                                     |
| ------------------------------ | ------------------------------------------------------------------------------------------------- |
| `--limit` (`list`, `asks`)      | Maximum rows to fetch (default 50 for `list`, 200 for `asks` when no mission id is given)        |
| `--policy` (`fire`)             | Envelope: the HITL policy bounding the unattended unit (default: `default-mission-policy` config) |
| `--wait` (`fire`)               | Block until the mission reaches a terminal status. **Required** — see below.                     |
| `--timeout` (`fire`)            | Maximum time to wait for a terminal status before tearing the unit down (default `30m`)           |
| `--reason` (`stop`)             | One line on why the mission is being stopped, persisted as the status reason                     |

`mission fire <agent> <intent...>` dispatches the fleet **in-process**: the unit is a child subprocess of this CLI invocation, so `--wait` is required — a detached fire from a one-shot CLI would tear its own mission down when the command exits. Fire-and-detach needs a long-lived session: `contenox beam`, an editor over `contenox acp`, or a host — each with the `/mission` command. Exit status is 0 when the mission lands; non-zero when it derails, gets stuck, is abandoned, or the wait times out.

> **Beta:** user-authored agents (custom `chain-agent-*` chain files, like the one declaring `agent-reviewer` above) require `contenox config set opt-in-beta true` (or `CONTENOX_OPT_IN_BETA=1`) and their interface may change; missions themselves and the shipped `agent-planner` work without it.

Answering a mission's pending question or permission gate is not a mission verb — use `contenox approvals respond`, which answers every pending ask in the system, mission-bound or not; `mission asks` only narrows the view to one mission (or every open one).

### `contenox approvals`

The durable ask inbox: list pending approvals and questions, and answer them. A gated tool call or a mission's question becomes a durable ask the moment it is raised, and the run checkpoints and releases its process rather than waiting; the ask is a row any process can answer later.

```bash
contenox approvals list
contenox approvals respond <ask-id> --approve
contenox approvals respond <ask-id> --deny
contenox approvals respond <ask-id> --answer "use the staging database"
```

| Flag (subcommand)      | Description                                                          |
| ------------------------ | ------------------------------------------------------------------------ |
| `--limit` (`list`)       | Maximum number of asks to list (default 50)                          |
| `--approve` (`respond`) | Approve a pending permission ask                                      |
| `--deny` (`respond`)    | Deny a pending permission ask                                         |
| `--answer` (`respond`)  | Answer a pending question (attention ask) with your own words        |
| `--as-agent <name>` (`respond`) | Beta: record the answer as given by the named agent instead of you; pair with `--answer` |

`respond` requires exactly one of `--approve`, `--deny`, or `--answer`, and it must match the ask's kind: a question takes `--answer`; a permission gate takes `--approve`/`--deny`. When the ask has a saved checkpoint, `respond` resumes the suspended run to completion in this process — and a process that cannot build an engine (no default model configured) is refused **before** anything is recorded: a checkpointed run's verdict is one-shot, so the ask stays pending and answerable from a terminal that can reach your models. `approvals list` is also the reconciling read: it applies expired asks' `on_timeout` verdicts, and it finishes any answered run a crashed resumer left behind (a resume claim goes stale after 10 minutes and is then picked up here).

> **Beta:** `--as-agent` requires `contenox config set opt-in-beta true` (or `CONTENOX_OPT_IN_BETA=1`); without the opt-in the flag is absent, not hidden.

`--as-agent <name>` attributes a question's answer to a named agent, and it is enforced against the mission envelope's attention bounds: it is refused when the ask belongs to no mission, when the envelope carries no `attention.allowAgentAnswers` grant, or when the mission's agent-answer bound is already spent — in every refusal the question waits for a human instead. An accepted agent answer counts against the bound, and the durable ask records which agent answered. See [who may answer a unit's question](/docs/guide/hitl/#who-may-answer-a-subagent-attention).

### `contenox inbox`

The durable operator inbox: reports (and blockers) a mission left behind with no live session to read them — distinct from `contenox approvals` (the live ask queue still waiting on a verdict).

```bash
contenox inbox list
contenox inbox list --all
contenox inbox show <id>
contenox inbox ack <id>
```

| Flag (subcommand) | Description                                                            |
| -------------------- | --------------------------------------------------------------------- |
| `--limit` (`list`)   | Maximum number of inbox items to list (default 50)                    |
| `--all` (`list`)     | Include acknowledged items too (default: unacknowledged only)          |

A mission dispatched directly by an operator (`contenox mission fire`, not from a chat session) has no session listening for its reports; a mission fired from a session whose process later ended has none anymore either. Either way, its reports land in the inbox instead of vanishing. `ack` marks an item read without deleting it.

### `contenox events`

> **Beta:** the event tier requires `contenox config set opt-in-beta true` (or `CONTENOX_OPT_IN_BETA=1`) and its interface may change; without it this command is hidden and no trigger file loads.

Operate the durable event-dispatch tier: internal domain events (mission reports, status changes, plan revisions, attention asks) land in a durable local log, and operator-authored `trigger-*.json` files fire task chains from them. See [Events & triggers (beta)](/docs/guide/events/) for the event shape, trigger authoring, and the exact guarantees.

```bash
contenox events dispatch                  # foreground: catch up, then follow live; Ctrl-C stops
contenox events dispatch --auto           # unattended: no terminal approval prompts
contenox events list --since 41           # events with nid > 41, in append order
contenox events firings --status error    # recorded firings: what dispatched, failed, or was refused
contenox events prune --keep-days 30      # drop whole day-partitions older than 30 days
```

| Flag (subcommand)          | Description                                                                                     |
| --------------------------- | ------------------------------------------------------------------------------------------------ |
| `--auto` (`dispatch`)       | Non-interactive mode: no terminal approval prompts; fired chains route through the trigger's policy (or the default) without a terminal ask |
| `--since` (`list`)          | List events with nid greater than this cursor (default 0: from the start of the log)             |
| `--limit` (`list`)          | Maximum events to list (default 50)                                                              |
| `--since` (`firings`)       | List firings for events with nid greater than this cursor (default 0)                            |
| `--status` (`firings`)      | Filter by status: `ok`, `error`, `refused`, or `running`                                         |
| `--trigger` (`firings`)     | Filter by trigger name                                                                           |
| `--limit` (`firings`)       | Maximum firings to list (default 50, ceiling 1000)                                               |
| `--keep-days` (`prune`)     | Keep partitions from the last N days; older ones are dropped (default 30)                        |
| `--yes` (`prune`)           | Skip the confirmation prompt                                                                     |

`dispatch` runs in the foreground and prints one line per firing; there is no daemon — keep it alive with tmux, systemd, or `nohup`. Each (trigger, event) pair fires at most once, including across restarts — with one recovery: a claim left `running` for two hours by a dead host is taken over on the next claim attempt and fired again. A chain failure is recorded on the firing and never stops the loop; events past hop 4 are refused so triggers cannot loop forever. `firings` lists the durable claim records both firing paths write — [the guide](/docs/guide/events/#inspecting-firings-events-firings) reads the statuses. `prune` is never automatic: retention runs only when you invoke it, as an O(1) table drop per day, leaving the dispatch cursor and firing records untouched.

### `contenox shell-env`

Manage the global environment variables contenox injects into the shells it spawns (`local_shell`, forwarded to the connected client's terminal), layered on top of the environment scrub so an injected value always wins. See [Least-privilege shell environment](/docs/guide/confinement/environment/) for the full design and current status.

```bash
contenox shell-env set HTTP_PROXY=http://proxy:3128 GOCACHE=/var/cache/go
contenox shell-env list
contenox shell-env unset HTTP_PROXY
```

Values are global (every spawned shell), stored as plain configuration, and read live — not a place for secrets.

### `contenox sandbox env`

Preview which environment-variable **names** a spawned shell would inherit under the currently configured scrub, evaluated against this process's own environment. Values are always withheld.

```bash
contenox sandbox env             # the agent-shell policy (SANDBOX_SHELL_SCRUB, default deny-secrets)
contenox sandbox env --terminal  # the interactive-terminal policy (SANDBOX_TERMINAL_SCRUB, default off)
```

| Flag | Description |
| ---- | ----------- |
| `--terminal` | Show the interactive-terminal policy instead of the agent-shell policy |

See [Least-privilege shell environment](/docs/guide/confinement/environment/) for the scrub modes and the `SANDBOX_*` environment variables that configure them.

### The `/mission` slash command

Missions are the dual of chat mode. In chat you prompt turn by turn and approve each gated action yourself. In mission mode you fire a one-line intent at a declared agent under an **envelope** — a HITL policy that bounds what it may do unattended — and keep working; the unit acts inside the envelope, and only crossing it costs your attention.

From inside a session (`contenox beam`, or an editor over `contenox acp`) fire a mission without leaving the conversation:

- `/mission` — fires nothing. Prints the grammar, the defaults in force, and every envelope on the policy search path with its character: what a call no rule matches does, the unattended tool-call ceiling, and whether an agent may answer the unit's questions.
- `/mission <intent>` — fires the configured `default-mission-agent` under the `default-mission-policy` envelope.
- `/mission <agent-name> <intent>` — fires the named agent instead.
- `/mission --policy <envelope> [agent-name] <intent>` — bounds this one mission under a different envelope. `--policy=<envelope>` is accepted too. Flags must come **before** the agent and intent, so a `--` inside an intent stays literal text.

Envelopes are discovered the way the runtime's policy loader resolves them — the workspace `.contenox/` first, then `~/.contenox/`, first match wins — so an operator-authored `hitl-policy-*.json` is offered beside the shipped presets, and a shadowing workspace copy is the one listed. A name that resolves to no file is refused in the session, with the available names listed, instead of dispatching a unit under a fallback nobody chose.

The two agent forms are the same shape, so contenox resolves the first token against the declared-agent registry: a hit is the named form, a miss means the whole line is the intent for the default agent. The confirmation states which agent was chosen, the envelope, where that envelope came from (`--policy` or `default-mission-policy`), and the envelope's character — so the bounds just accepted are in the transcript, not only in a config file.

> **Beta:** naming a user-authored agent (a custom `chain-agent-*` chain) requires `contenox config set opt-in-beta true` (or `CONTENOX_OPT_IN_BETA=1`) and its interface may change; `/mission` itself and the shipped `agent-planner` work without it.

The dispatch runs **in-process**: the fired unit is a child subprocess of the calling session's own process, no daemon is needed, and the unit's reports stream live back into the firing session as they land. A mission with no agent or no envelope is refused. The hardened `acpx` profile never offers `/mission`.

The [oracle](/docs/use-cases/auto-attention/) needs no `/mission` equivalent: it mounts on the ACP host itself, from `contenox config set default-oracle-chain`, so every subagent this session fires — through `/mission`, `/plan`, or the `mission_start` tool — is already covered. Whether it may rule on a given subagent's asks is the envelope's `attention` bounds, not a per-command flag.

### The `/pair` and `/unpair` slash commands

Pairing attaches the machine to a relay, so the sessions this process serves can be reached from somewhere else — the [contenox app](https://app.contenox.com) on a phone, typically. A pairing describes the **machine**, so the credential lands in `~/.contenox/relay.json` and every contenox process on that machine uses it; these slash commands and the [`contenox pair`](#contenox-pair--contenox-unpair) CLI verbs are two entry points to the same stored pairing.

From inside a session — `contenox beam`, or an editor over `contenox acp`:

- `/pair <key>` — redeem a key minted in the app (**Pair device**) against the hosted relay whose address ships in the binary.
- `/pair <key> <endpoint>` — redeem against a relay you run yourself; the `CONTENOX_RELAY_ENDPOINT` environment variable sets the same thing for every `/pair` without an inline endpoint.
- `/pair` — report what this machine is attached to (relay, instance, account), changing nothing. It never prints the credential.
- `/unpair` — delete the stored credential, so this machine stops dialling. Local only: revoking the instance is done in the app, and a revoked machine is refused at its next dial whether or not it still holds the file.
- `/link` — print the link that opens **this session** in the app, so a session started at the desk can be picked up on a phone. It is just a URL — opening it still requires signing in to the account this machine is paired to. On an unpaired machine it points you at `/pair` instead.

What is sent when a key is redeemed (the key and this machine's hostname, nothing else), what lands in `~/.contenox/relay.json`, and how the relay's identity is verified from then on: [Pairing a machine with a relay](/docs/guide/pairing/).

### `contenox state`

Inspects captured execution state from past chain runs — the per-task steps, handlers, transitions, and timings recorded for each request.

```bash
contenox state list             # list request IDs with captured execution state
contenox state show <reqID>     # print the captured steps for a request
contenox state show <reqID> --raw   # print the raw captured state as JSON
```

### `contenox cache clear`

Clears cached backend model lists so they're refetched from the live backends next time they're needed. Use it after adding models to a backend that the runtime hasn't picked up yet.

```bash
contenox cache clear
```

### `contenox update`

Updates `contenox` to the latest release, or just checks for one.

```bash
contenox update             # download and install the latest release
contenox update check       # report whether a newer version exists, without installing
```

### `contenox acp` / `contenox acpx`

Run Contenox as an [ACP](https://agentclientprotocol.com/) agent over stdio, for editor/desktop clients (Zed, JetBrains, AionUi, OpenClaw). `acp` uses the standard editor profile (gated tools route through the client's approval UI); `acpx` uses the hardened headless / untrusted-driver profile.

```bash
contenox acp                 # standard editor profile
contenox acp --auto          # unattended: disable HITL permission prompts
contenox acp --setup         # run the setup wizard, then exit (no server started)
contenox acpx                # headless / untrusted-driver profile
```

| Flag                | Description                                                                                    |
| --------------------- | -------------------------------------------------------------------------------------------- |
| `--auto`             | Non-interactive mode: disable HITL permission prompts (gated tools run unattended)             |
| `--setup`            | Run the interactive setup wizard to configure provider and model, then exit                    |
| `--workspace-id <id>` | Workspace ID for new ACP sessions (default: the stable workspace from `~/.contenox/workspace.id`, same as the CLI) |

Each profile's chain resolves in order: an operator copy at `~/.contenox/<name>.json`, then a compiled `~/.contenox/.generated/<name>.json`, then the shipped `~/.contenox/system/<name>.json` — first match wins. `CONTENOX_ACP_CHAIN_PATH` (acp) and `CONTENOX_ACPX_CHAIN_PATH` (acpx) override this for one run. See the [editor integration guides](/docs/integrations/editors/zed/) for client setup.

### `contenox serve [path]`

Run contenox as a long-lived host — the organization's shape: a standing process on a box nobody is sitting at, reachable from the [contenox app](https://app.contenox.com) through the relay.

Where `acp` serves one client over stdio and `beam` serves the person who started it, `serve` has no client of its own: the relay tunnel is its inbound path, so it checks its setup, prints a status screen, and stays up until interrupted.

```bash
contenox serve              # the workspace is your home directory
contenox serve .            # the directory you are standing in
contenox serve ~/src/api    # one project
```

The optional path is **the** workspace this instance serves, fixed for the life of the process — see [Workspace authority](#workspace-authority). With no path the host serves your home directory: a host outlives the shell that started it and is reached from a device that knows nothing about that shell's working directory, so scoping it to the launch directory would make its scope depend on where you happened to be standing. `contenox serve .` asks for the narrow scope explicitly.

A host has **no `local_fs` and no `local_shell`**, under any policy. Those tools are forwarded to a connected client's `fs/*` and `terminal/*` capabilities, and a standing host has no such client; every capability it has is an MCP server or OpenAPI service you attached. See [contenox serve: the standing host](/docs/guide/serve/).

The status screen reports what the process actually is — setup readiness (the same check `contenox doctor` runs), the workspace, the model, the relay and app URL when paired, and the log directory with the retention bounds in force. An unpaired host says so and prints the steps to pair it; it still runs, it is simply reachable on that machine only.

| Flag           | Description                                                              |
| -------------- | ------------------------------------------------------------------------ |
| `--log-dir <dir>` | Write host logs here (default: `<data-dir>/logs`)                     |

Structured logs go to the log directory rather than the screen, so the screen stays a status display. Files are named `serve-<YYYY-MM-DD>.log`, and a day that outgrows its size bound continues in `serve-<YYYY-MM-DD>.2.log`, `.3.log`, and so on. Retention is bounded by the `log-*` [config keys](/docs/reference/config/#set-persistent-defaults); restarting a host continues the current part rather than starting a new file per launch.

Running a host: [contenox serve: the standing host](/docs/guide/serve/).

### `contenox pair` / `contenox unpair`

Attach this machine to a relay, or detach it, without opening an editor session. Same stored pairing as the [`/pair` slash command](#the-pair-and-unpair-slash-commands) — a pairing describes the machine, so whichever entry point writes it, every later process finds it.

```bash
contenox pair                    # what is this machine attached to?
contenox pair K7M-3PQ            # redeem a key minted in the app
contenox pair K7M-3PQ https://relay.example.internal   # a relay you run yourself
contenox unpair                  # delete the stored credential
```

- `contenox pair` with no key reports the relay, instance and account, and the app URL. It never prints the credential.
- The key is short-lived and redeemable exactly once; mint a new one in the app (**Pair device**) if it expires.
- A self-hosted relay hands out its own public key at redemption and is verified against that key from then on. `CONTENOX_RELAY_ENDPOINT` sets the same endpoint for every `pair` without an inline one.
- `contenox unpair` is local: it stops this machine dialling but does not revoke. Revoke an instance in the app — a revoked machine is refused at its next dial whether or not it still holds the file.

Pairing alone attaches the machine; run [`contenox serve`](#contenox-serve-path) — or keep a [`contenox beam`](#contenox-beam) session open — to keep it reachable.

### `contenox autocomplete --stdio`

Serve fill-in-the-middle code completions over a JSON-lines stdio protocol, for editor integrations that want completions without a full ACP session. Uses the `default-autocomplete-model` / `default-autocomplete-provider` config role — the same keys the ACP editor surface reads — and refuses to start (nonzero exit, error naming the key) when no autocomplete model is configured.

```bash
contenox config set default-autocomplete-model qwen2.5-coder:7b
contenox config set default-autocomplete-provider ollama
contenox autocomplete --stdio
```

One JSON object per line, requests on stdin, responses on stdout. Responses may arrive out of order — match by `id`; a client that moved on simply ignores stale ids (there is no cancellation):

```json
{"id": "1", "path": "main.go", "language": "go", "prefix": "func main() {\n\t", "suffix": "\n}", "max_tokens": 64}
{"id": "1", "completion": "fmt.Println(\"hello\")"}
```

A failed or invalid request answers `{"id": "...", "error": "..."}` on the same stream (a malformed line answers with an empty `id`). `path` and `language` are accepted but do not change the prompt. `prefix`/`suffix` should be caller-truncated; the server accepts up to 16 KiB per side and truncates longer sides toward the cursor position, noting it on stderr. `max_tokens` defaults to 128; each completion is bounded by a 20-second budget.

### `contenox version`

Prints the current binary version and exits.

```bash
contenox version
```

## Environment variables

| Variable | Description |
|---|---|
| `CONTENOX_ACP_CHAIN_PATH` | Override the chain file used by `contenox acp` sessions |
| `CONTENOX_ACPX_CHAIN_PATH`| Override the chain file used by headless ACPX sessions |
| `CONTENOX_DEFAULT_MODEL` / `CONTENOX_DEFAULT_PROVIDER` | Process-level override of the configured default model/provider (nothing is persisted). Also the ACP `env_var` auth-method contract for non-interactive setup. |
| `CONTENOX_DEFAULT_ALT_MODEL` / `CONTENOX_DEFAULT_ALT_PROVIDER` | Same, for the alt model pair. |
| `CONTENOX_DEFAULT_MAX_TOKENS` / `CONTENOX_DEFAULT_THINK` | Same, for the response token cap and reasoning level. |
| `CONTENOX_BASE_URL` | Endpoint URL for account-specific providers whose URL cannot be defaulted (e.g. Vertex: project + region). |
| `CONTENOX_OPT_IN_BETA` | Per-invocation override of the `opt-in-beta` config key (`1`/`true` enables the beta features, any other value disables them; unset falls back to config). |
| `CONTENOX_RELAY_ENDPOINT` | The relay `/pair` redeems against when none is given inline, instead of the hosted relay compiled into the binary — see [Pairing a machine with a relay](/docs/guide/pairing/). |
| `CONTENOX_SANDBOX_NETWORK_WALL` | Set to `1` to build the [agent sandbox](/docs/guide/confinement/sandbox/)'s network wall with no route at all, for a fully offline foreign agent. |
| `CONTENOX_POSTGRES_URL` | Move the store off the SQLite file onto a Postgres database. Requires `CONTENOX_NATS_URL` and `CONTENOX_VALKEY_URL` too — see [External backends for state](/docs/reference/config/#external-backends-for-state-opt-in). |
| `CONTENOX_NATS_URL` | Move the message bus off the database onto a NATS server (`nats://host:4222`; comma-separate a server list). |
| `CONTENOX_VALKEY_URL` | Move the key-value cache off the database onto a Valkey server (`valkey://host:6379`, or a bare `host:6379`). Add a user, a database index and a key namespace to keep it out of the way of whatever else uses that server: `valkey://appuser:secret@host:6379/3?namespace=contenox` — see [Isolating contenox inside a Valkey you already run](/docs/reference/config/#isolating-contenox-inside-a-valkey-you-already-run). |

`SANDBOX_SHELL_SCRUB`, `SANDBOX_TERMINAL_SCRUB`, `SANDBOX_ENV_ALLOW`, and `SANDBOX_ENV_DENY` configure the shell environment scrub — see [Least-privilege shell environment](/docs/guide/confinement/environment/) for their modes and current status.
