---
title: Contenox CLI Reference
description: Every contenox subcommand, flag, and environment variable.
---

# Contenox CLI Reference

`contenox` is the local AI agent CLI. It runs the Contenox chain engine entirely on your machine.

![A natural-language task in the terminal: contenox reads the repo and answers](/hero.gif)

## Global Flags

Persistent flags on the root command (also shown under **Global Flags** on subcommands). Run `contenox --help` for the full list.

| Flag                             | Description                                                                                                                       |
| -------------------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| `--model <name>`                 | Model override for this invocation; persistent default is `contenox config set default-model <name>`                               |
| `--provider <type>`              | Provider override for this invocation. See `contenox backend add --help` for supported backend types. |
| `--db <path>`                    | SQLite DB path (default: `~/.contenox/local.db`). The one global database is shared by every workspace. |
| `--data-dir <path>`              | Override the `.contenox` data directory (skips walk-up search). Used to locate the workspace's `workspace.id` and chain files; does not change the database location. |
| `--timeout`                      | Max execution time per invocation (default `5m`)                                                                                  |
| `--context`                      | Context length hint for the tokenizer                                                                                             |
| `--ollama`                       | Ollama base URL (default `http://127.0.0.1:11434`)                                                                                |
| `--no-delete-models`             | Legacy compatibility flag; a no-op in the OSS runtime (model deletion is disabled). Defaults to **true**.                          |
| `--chain <path>`                 | Chain JSON for injected `run` / chat when applicable                                                                              |
| `--input <value>`                | Input string or `@file` (chat / bare run paths)                                                                                   |
| `--trace`                        | Structured operation telemetry on stderr                                                                                          |
| `--steps`                        | Print execution steps after the result                                                                                            |
| `--think <level>`                | Set reasoning level for supported models: `auto`, `off`, `minimal`, `low`, `medium`, `high`, `xhigh`                              |
| `--raw`                          | Print full structured output (e.g. entire chat JSON)                                                                              |
| `--shell`                        | Enable `local_shell` tools (trusted environments only)                                                                             |
| `--local-exec-allowed-dir <dir>` | Restrict local filesystem access and `local_shell` executable/script paths for this invocation                                    |
| `--alt-model <name>`             | Alt model name (for chains referencing `{{var:alt_model}}`). Overrides `default-alt-model` config.                                  |
| `--alt-provider <type>`          | Alt provider type (for chains referencing `{{var:alt_provider}}`). Overrides `default-alt-provider` config.                          |
| `--max-tokens <N>`               | Response token cap (for chains referencing `{{var:max_tokens}}`). Overrides `default-max-tokens` config.                             |
| `-e, --editor`                   | Open `$EDITOR` (or `$VISUAL`, fallback `nano`) to compose the prompt.                                                             |

## Subcommands

### `contenox setup`

Runs an interactive setup wizard to configure your primary provider, model, and API key. This is the recommended first step for all new users. It ensures your global `~/.contenox/` configuration is ready for use.

```bash
contenox setup
```

The wizard guides you through picking a provider (Ollama, OpenAI, Gemini, or Vertex AI), entering an API key or base URL where needed, and setting your first default model. It needs a real terminal (reads answers from stdin) and will not guess a default from a closed or piped stdin.

### `contenox` (bare — stateful `chat`)

If the first token is **not** a reserved subcommand (`chat`, `init`, `run`, …), the CLI **prepends `chat`**. This starts or continues a stateful, session-backed conversation. It is the default, interactive mode.

The default chat chain is resolved by name: workspace `.contenox/default-chain.json` wins when present, otherwise Contenox falls back to `~/.contenox/default-chain.json`.

```bash
contenox "what can you do?"
echo "summarise README.md" | contenox
contenox --shell "list files here"
contenox --local-exec-allowed-dir . "summarise the README"
```

### `contenox chat`

Sends a message to the **active chat session** and prints the response. History is persisted across invocations in SQLite. This is the explicit version of the bare `contenox` command.

```bash
contenox chat "what can you do?"
echo "summarise README.md" | contenox chat
contenox chat --shell "list files here"
contenox chat --attach ./screenshot.png "what is in this image?"
```

| Flag                             | Description                                                            |
| -------------------------------- | ---------------------------------------------------------------------- |
| `--trim N`                       | Only send last N messages from session history to the model (0 = all)  |
| `--attach <path>`                | Attach an image to this message (repeatable). Routes the turn to a vision-capable model; a non-image file (sniffed from its bytes, not its extension) is rejected, and files over 10 MiB are refused. |
| `--last N`                       | Print last N user/assistant turns after the reply (0 = only new reply) |
| `--shell`                        | Enable `local_shell` tools                                              |
| `--local-exec-allowed-dir <dir>` | Restrict local filesystem access and shell executable/script paths       |
| `--auto`                         | Disable HITL prompts for this invocation. HITL is on by default.        |

<h3 id="sessions"><code>contenox session</code></h3>

Manage named chat sessions. Each session maintains its own conversation history. `list` and `show` default to the active scope; the whole database can also be inspected across workspaces and namespaces, and any session opened directly by id — useful for recovering a session an editor lost track of.

```bash
contenox session list                    # list all sessions (* = active)
contenox session new [name]             # create a session (becomes active)
contenox session switch <name>          # switch to a different session
contenox session show                   # show active session's history
contenox session show <name>            # show any session by name
contenox session show <id>              # show any session by id (any workspace)
contenox session show --tail 10         # show last 10 messages
contenox session show --head 5          # show first 5 messages
contenox session show default --tail 6  # tail a non-active session
contenox session fork [name]            # copy the active session to a new one (becomes active)
contenox session fork --summary         # compact older history into a summary, then fork and continue
contenox session fork --summary --keep 12  # keep the last 12 messages verbatim (default: 8)
contenox session delete <name>          # delete session and all messages
```

`session fork` branches the current conversation into a new session so you can explore an alternate direction without losing the original. `--summary` first compacts the older turns into a summary (via `chain-compact.json`) before forking, which trims a long history while preserving context; `--keep` sets how many of the most recent messages stay verbatim instead of being summarized.

Inspect the whole database, not just the active workspace/identity:

```bash
contenox session workspaces              # list workspaces and namespaces (counts)
contenox session list --all              # every session across the whole DB
contenox session list --workspace <id>   # sessions in a workspace
contenox session list --namespace <ns>   # sessions in a namespace (e.g. jetbrainsgoland)
```

A namespace is the session-name prefix before its generated id (e.g. `jetbrainsgoland`, `zed`, `default`). To recover a session an editor abandoned: find it with `session list --namespace <ns>`, then `session show <id>`.

### `contenox run`

Executes a chain non-interactively. Unlike `chat`, `run` does not use session history. It is for stateless, one-shot chain executions.

```bash
contenox run --chain .contenox/chain-nws.json --input-type chat "how is the weather?"
contenox run --chain .contenox/my-chain.json --shell "refactor main.go"
```

- `--chain <path>`: Optional if `<resolved .contenox>/default-run-chain.json` exists; otherwise required.
- `--input-type <type>`: `string` (default), `chat`, `json`, `int` — see `contenox run --help`.
- `--shell`: Enable shell execution for this invocation (use only in trusted environments).
- `--auto`: Disable HITL approval prompts for non-interactive runs. Default is HITL on.
- `--think` / `--trace` / `--steps`: Global flags (see table above).

### `contenox beam`

The Contenox terminal UI: chat, plan, and shell in one persistent session, in a full-screen TUI.

```bash
contenox beam                 # open in the current directory
contenox beam ~/src/myproject # open in a specific directory
contenox beam --session zed-a1b2c3
contenox beam --plain         # no color or unicode, ASCII glyphs only
```

| Flag              | Description                                                                 |
| ----------------- | ---------------------------------------------------------------------------- |
| `--session <name>` | Open the named session instead of the last active one                       |
| `--light`          | Render for a light terminal background (overrides automatic detection)      |
| `--plain`          | Drop all color and unicode: ASCII glyphs, no styling                        |

The beam TUI requires a real terminal on stdout; it refuses to start on a non-TTY. Unlike `chat`, `local_shell` is enabled by default here (pass `--shell=false` to disable). It also supports the `/mission` slash command the same way an ACP editor session does — see [The `/mission` slash command](#the-mission-slash-command) below.

`contenox beam` loads its own chain, `~/.contenox/default-beam-chain.json` (override with `CONTENOX_BEAM_CHAIN_PATH`), and its own HITL envelope, `hitl-policy-beam.json` — a copy of the editor profile's policy tuned for the attended terminal UI (see [HITL Policies](/docs/guide/hitl/#built-in-presets)).

### `contenox doctor`

Prints local LLM setup readiness: default model, default provider, and backend reachability.

```bash
contenox doctor
contenox doctor --json          # machine-readable output
contenox doctor --skip-cycle    # faster; skips backend sync (status may be stale)
```

| Flag            | Description                                              |
| --------------- | ---------------------------------------------------------- |
| `--json`        | Print results as JSON instead of human-readable text     |
| `--skip-cycle`  | Skip syncing backends before the check (faster but may show stale status) |

`doctor` also reports vision-capable model availability, flags a HITL policy preset that predates the currently shipped toolset (fix with `contenox init --refresh-policies`), and warns — without changing anything — when `default-max-tokens` exceeds the active provider's output-token ceiling.

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
contenox model set-context qwen2.5:7b           --context 32k
contenox model set-context gpt-5-mini           --context 128k
contenox model set-context gemini-3.1-pro-preview --context 1m
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

Inspect and manage the runtime's declared agents. An agent is one of the runtime's own [task chains](/docs/guide/first-chain/), addressable and spawnable as an ACP peer. Agents are registered automatically by chain-agent discovery from the chain files on disk — this command inspects them, toggles their enabled state, and removes stale registrations. Declared agents are what `/mission` and `contenox mission fire` dispatch.

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

### `contenox init [provider]`

Initializes a workspace (`.contenox/`) and ensures default runtime presets exist globally (`~/.contenox/`). It's best to run `contenox setup` first for a guided configuration.

`init` creates the `.contenox/workspace.id` marker — a project's portable identity. The marker carries a stable workspace UUID (the database scoping token every session under the project is filed under) plus an optional friendly **name**. It travels *with* the directory, so a project means one thing to the CLI and every ACP session alike. Default chains and HITL policies are written under `~/.contenox/` unless they already exist. Workspace-local `.contenox/` files can override these global presets by name.

By default `init` walks up to reuse an ancestor's `.contenox` if one exists (like `git`). Pass `--project` to force a *fresh* project marker in the current directory instead — a distinct workspace nested under a larger one — and `--name` to give it a friendly name (default: the folder's own name). Marking a project does not by itself let sessions open it; `init --project` prints the `contenox workspace add` line that grants it.

You can optionally specify a provider to pre-configure defaults.

```bash
contenox init                          # scaffold with default chains
contenox init gemini                   # pre-configure for Gemini
contenox init openai                   # pre-configure for OpenAI
contenox init --force                  # overwrite existing files
contenox init --update                 # refresh unchanged default files
contenox init --refresh-policies       # rewrite only the HITL policy presets
contenox init --project --name "API"   # a fresh named project in the current dir
```

| Flag        | Description                         |
| ----------- | ----------------------------------- |
| `-f, --force` | Overwrite existing preset files |
| `--update`  | Refresh unchanged default files to the latest embedded versions |
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
contenox config set default-alt-model gemini-2.5-flash
contenox config set default-alt-provider gemini
contenox config set default-autocomplete-model qwen2.5-coder:7b
contenox config set default-autocomplete-provider ollama
contenox config set default-embed-model nomic-embed-text
contenox config set default-embed-provider ollama
contenox config set default-max-tokens 8192
contenox config set default-think high
contenox config set default-chain    .contenox/default-chain.json
contenox config set hitl-policy-name hitl-policy-strict.json

contenox config get default-model
contenox config list
```

Valid global keys: `default-model`, `default-provider`, `default-alt-model`, `default-alt-provider`, `default-autocomplete-model`, `default-autocomplete-provider`, `default-embed-model`, `default-embed-provider`, `default-max-tokens`, `default-think`, `telemetry-enabled`, `update-check`, `default-mission-agent`, `default-mission-policy`, `fleet-max-parallel`.

Valid workspace keys: `default-chain`, `hitl-policy-name`.

| Key | Description |
|---|---|
| `default-embed-model` | Embedding model used by `contenox index` / `contenox search`. Unset falls back to `default-model`, which embeds only on some providers. |
| `default-embed-provider` | Provider type for the embedding model, independent from `default-provider`. Unset uses `default-provider`. |
| `default-mission-agent` | Declared agent the ACP `/mission <intent>` slash command falls back to when none is named. `contenox mission fire` always requires the agent name as a positional argument, so this key does not affect it. |
| `default-mission-policy` | Envelope (HITL policy) name that both `/mission` and `contenox mission fire --policy` fall back to when none is named. |
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

> [!NOTE]
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

`mission fire <agent> <intent...>` dispatches the fleet **in-process**: the unit is a child subprocess of this CLI invocation, so `--wait` is required — a detached fire from a one-shot CLI would tear its own mission down when the command exits. Fire-and-detach needs a long-lived host: an editor session (`contenox acp`, the `/mission` command) or `contenox beam`. Exit status is 0 when the mission lands; non-zero when it derails, gets stuck, is abandoned, or the wait times out.

Answering a mission's pending question or permission gate is not a mission verb — use `contenox approvals respond`, which answers every pending ask in the system, mission-bound or not; `mission asks` only narrows the view to one mission (or every open one).

### `contenox approvals`

The durable ask inbox: list pending approvals and questions, and answer them. A gated tool call or a mission's question parks for a short window, then checkpoints the run and releases its process; the ask stays a durable row any process can answer later.

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

`respond` requires exactly one of `--approve`, `--deny`, or `--answer`, and it must match the ask's kind: a question takes `--answer`; a permission gate takes `--approve`/`--deny`. When the ask has a saved checkpoint, `respond` resumes the suspended run to completion in this process if it can build an engine (a default model must be configured); otherwise the verdict is recorded and the run resumes the next time a capable process answers or sweeps.

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

### `contenox index [dir]` / `contenox search <question>`

Build a semantic index over a workspace, then ask it questions with file:line-range citations. The same index backs the agent's `workspace_search` tool.

```bash
contenox config set default-embed-model nomic-embed-text  # needed once; most chat models cannot embed

contenox index                          # build/refresh the index for the current directory
contenox index ~/src/project
contenox index --force                  # re-embed every file, not just changed ones
contenox index --yes                    # skip the cost confirmation (scripts, CI)

contenox search "where is retry backoff configured"
contenox search "how does the approval flow work" --top 3
contenox search "session storage" --json | jq -r '.[].path'
```

| Flag (command)     | Description                                                                          |
| -------------------- | ------------------------------------------------------------------------------------- |
| `--force` (`index`) | Re-embed every file instead of only the changed ones                                 |
| `--yes` (`index`)   | Skip the cost confirmation (required when stdin is not a terminal)                   |
| `--top` (`search`)  | Maximum hits to return (default 8, ceiling 50)                                        |
| `--json` (`search`) | Emit the hits as JSON for scripting (empty result is `[]`, never `null`)              |
| `--dir` (`search`)  | Workspace directory to search (default: current directory)                           |

Indexing costs one embedding call per chunk, so `contenox index` always plans and prints the cost — file, chunk, and embed-call counts — before spending it, and asks for confirmation unless `--yes` is passed or nothing changed. Refreshes are incremental: only changed files are re-embedded, and files that disappeared drop their chunks for free. `contenox search` reads only the existing index — it never touches the filesystem live, and a hit whose file changed since indexing is marked `STALE` rather than served as current.

### `contenox workspace`

Grant or revoke the **workspace roots** a session may run in — the directories a chat, a fired mission unit, or an ACP session may choose as its working directory. Granting a root grants everything **under** it; a directory outside every granted root (a sibling, a prefix-trick neighbour like `/home/meX` against `/home/me`, or a symlink whose real target escapes) is refused. A too-broad root — the filesystem root, your home directory, or a top-level system directory like `/srv` — is also refused, so a grant can never hand a session an entire home or disk; grant the specific project directory.

```bash
contenox workspace add /home/me/src              # grant a root (and everything under it)
contenox workspace add /home/me/api --name "API" # grant AND name the project
contenox workspace list                           # the roots you have granted
contenox workspace remove /home/me/scratch        # revoke a grant
```

Granting a root also **registers it as a project**: `add` writes (or reuses) the folder's `.contenox/workspace.id` marker, so a root added here shows the same friendly name in `list`. `--name` sets that name (default: the folder's own name); re-running `add` on an already-granted path with a new `--name` **renames** the project without changing its workspace id, so its existing sessions stay attached. This is the exact same marker stamp `init --project` applies — one on-disk result across both entry points.

A grant is durable config in the shared database (`~/.contenox/local.db`), so `add`/`remove` write it directly and every session — CLI, TUI, or ACP — reads the same grants.

`add` requires the path to be an existing directory (a workspace root must be a real directory); `remove` does not, so a grant to a since-deleted directory can be cleaned up. Both are idempotent. `list` prints the durable grants these verbs manage, each with its project name when set.

| Flag | Description |
| ---- | ----------- |
| `--name <name>` (on `add`) | Friendly project name stamped into the folder's marker; re-adding with a new name renames the project |

### `contenox shell-env`

Manage the global environment variables contenox injects into the shells it spawns (`local_shell`, the `shell_session` PTY, and the interactive terminal), layered on top of the environment scrub so an injected value always wins. See [Least-privilege shell environment](/docs/guide/environment-scrubbing/) for the full design and current status.

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

See [Least-privilege shell environment](/docs/guide/environment-scrubbing/) for the scrub modes and the `SANDBOX_*` environment variables that configure them.

### The `/mission` slash command

Missions are the dual of chat mode. In chat you prompt turn by turn and approve each gated action yourself. In mission mode you fire a one-line intent at a declared agent under an **envelope** — a HITL policy that bounds what it may do unattended — and keep working; the unit acts inside the envelope, and only crossing it costs your attention.

From inside a session (`contenox acp`, or the beam TUI) fire a mission without leaving the conversation:

- `/mission <intent>` — fires the configured `default-mission-agent` under the `default-mission-policy` envelope.
- `/mission <agent-name> <intent>` — fires the named agent instead.

The two forms are the same shape, so contenox resolves the first token against the declared-agent registry: a hit is the named form, a miss means the whole line is the intent for the default agent. The confirmation always states which agent was chosen and echoes the intent, so a misread is visible immediately.

The dispatch runs **in-process**: the fired unit is a child subprocess of the calling session's own process, no daemon is needed, and the unit's reports stream live back into the firing session as they land. A mission with no agent or no envelope is refused. The hardened `acpx` profile never offers `/mission`.

### `contenox state`

Inspects captured execution state from past chain runs — the per-task steps, handlers, transitions, and timings recorded for each request.

```bash
contenox state list             # list request IDs with captured execution state
contenox state show <reqID>     # print the captured steps for a request
contenox state show <reqID> --raw   # print the raw captured state as JSON
```

### `contenox cache clear`

Clears cached backend model lists so the next `chat`/`run` refetches them from the live backends. Use it after adding models to a backend that the runtime hasn't picked up yet.

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

The chain each profile loads is overridable via `CONTENOX_ACP_CHAIN_PATH` (acp) and `CONTENOX_ACPX_CHAIN_PATH` (acpx). See the [editor integration guides](/docs/integrations/editors/zed/) for client setup.

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
| `CONTENOX_BEAM_CHAIN_PATH` | Override the chain file used by `contenox beam` (default `~/.contenox/default-beam-chain.json`) — beam drives the same in-process ACP transport an editor session does, but resolves its own chain file and env var independently of `CONTENOX_ACP_CHAIN_PATH` |
| `CONTENOX_DEFAULT_MODEL` / `CONTENOX_DEFAULT_PROVIDER` | Process-level override of the configured default model/provider (nothing is persisted). Also the ACP `env_var` auth-method contract for non-interactive setup. |
| `CONTENOX_DEFAULT_ALT_MODEL` / `CONTENOX_DEFAULT_ALT_PROVIDER` | Same, for the alt model pair. |
| `CONTENOX_DEFAULT_MAX_TOKENS` / `CONTENOX_DEFAULT_THINK` | Same, for the response token cap and reasoning level. |
| `CONTENOX_BASE_URL` | Endpoint URL for account-specific providers whose URL cannot be defaulted (e.g. Vertex: project + region). |
| `CONTENOX_SANDBOX_NETWORK_WALL` | Set to `1` to build the [agent sandbox](/docs/guide/agent-sandbox/)'s network wall with no route at all, for a fully offline foreign agent. |

`SANDBOX_SHELL_SCRUB`, `SANDBOX_TERMINAL_SCRUB`, `SANDBOX_ENV_ALLOW`, and `SANDBOX_ENV_DENY` configure the shell environment scrub — see [Least-privilege shell environment](/docs/guide/environment-scrubbing/) for their modes and current status.
