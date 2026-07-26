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

The wizard will guide you through picking a provider (like Ollama, OpenAI, or Gemini), entering an API key if required, and setting your first default model.

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
```

| Flag                             | Description                                                            |
| -------------------------------- | ---------------------------------------------------------------------- |
| `--trim N`                       | Only send last N messages from session history to the model (0 = all)  |
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
contenox session delete <name>          # delete session and all messages
```

`session fork` branches the current conversation into a new session so you can explore an alternate direction without losing the original. `--summary` first compacts the older turns into a summary (via `chain-compact.json`) before forking, which trims a long history while preserving context.

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

### `contenox doctor`

Prints local LLM setup readiness: default model, default provider, and backend reachability.

```bash
contenox doctor
contenox doctor --json          # machine-readable output
contenox doctor --skip-cycle    # faster; skips backend sync (status may be stale)
```

| Flag            | Description                                              |
| --------------- | -------------------------------------------------------- |
| `--json`        | Print results as JSON instead of human-readable text     |
| `--skip-cycle`  | Skip syncing backends before the check (faster but may show stale status) |

### `contenox model`

Manage model configuration: the local **Model Registry** (a name-to-URL index of model entries), live model listings from configured backends, context-window overrides, and capability overrides.

#### `contenox model registry-list`

List all curated and user-added registry entries. Does not require a running backend.

```bash
contenox model registry-list
```

#### `contenox model add`

Register a custom model entry in the local registry without downloading.

```bash
contenox model add my-model --url https://huggingface.co/org/repo/resolve/main/model.gguf
contenox model add my-model --url https://... --size 4500000000
```

| Flag     | Description                                  |
| -------- | -------------------------------------------- |
| `--url`  | Source URL (required)                        |
| `--size` | File size in bytes (optional, informational) |

#### `contenox model show`

Display registry details for a model.

```bash
contenox model show qwen3-4b
```

#### `contenox model remove`

Remove a user-added registry entry by name. Curated entries cannot be removed.

```bash
contenox model remove my-model
```

#### `contenox model list`

List models currently available from all configured backends (live query, requires at least one backend).

```bash
contenox model list
```

#### `contenox model set-context`

Override the context window size for a specific model name. Useful when a backend reports a different (or no) context size than the model actually supports.

```bash
contenox model set-context qwen2.5:7b           --context 32k
contenox model set-context gpt-5-mini           --context 128k
contenox model set-context gemini-3.1-pro-preview --context 1m
```

| Flag        | Description                                                      |
| ----------- | ---------------------------------------------------------------- |
| `--context` | Context window size: bare integer or shorthand (`12k`, `128k`, `1m`). Required. |

#### `contenox model capability`

Manage manual provider/model capability overrides — the reasoning (`think`) and image-input (`vision`) capabilities the runtime assumes for a given provider/model when the catalog doesn't declare them.

```bash
contenox model capability set   <provider> <model> --think true   # mark the model as supporting reasoning
contenox model capability set   <provider> <model> --vision true  # mark the model as accepting image input
contenox model capability show  <provider> <model>                # show the current override
contenox model capability unset <provider> <model>                # remove the override (revert to catalog)
```

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

### `contenox agent`

Inspect and manage the runtime's declared agents. An agent is one of the runtime's own [task chains](/docs/guide/first-chain/), addressable and spawnable as an ACP peer. Agents are registered automatically by chain-agent discovery from the chain files on disk — this command inspects them, toggles their enabled state, and removes stale registrations. Declared agents are what `/mission` fires.

```bash
contenox agent list                       # id, name, source, kind, enabled
contenox agent show agent-reviewer        # provenance + config_json
contenox agent enable agent-reviewer      # (and: disable)
contenox agent remove agent-reviewer      # (alias: rm)
```

`remove` deletes only the local registration; discovery may re-register it on the next startup if its chain file still exists.

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
contenox init --project --name "API"   # a fresh named project in the current dir
```

| Flag        | Description                         |
| ----------- | ----------------------------------- |
| `-f, --force` | Overwrite existing preset files |
| `--update`  | Refresh unchanged default files to the latest embedded versions |
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
| `--type`        | Backend type: `ollama`, `openai`, `anthropic`, `gemini`, `bedrock`, `vllm`, `vertex-google`. |
| `--url`         | Base URL (auto-inferred for openai/anthropic/gemini; required for vllm, bedrock, and vertex-google) |
| `--api-key-env` | Environment variable holding the API key (preferred)                                      |
| `--api-key`     | API key literal (avoid — use `--api-key-env`)                                             |

### `contenox config`

Manage persistent CLI defaults stored in SQLite.

```bash
contenox config set default-provider ollama
contenox config set default-model    qwen3:8b
contenox config set default-alt-model gemini-2.5-flash
contenox config set default-alt-provider gemini
contenox config set default-autocomplete-model qwen2.5-coder:7b
contenox config set default-autocomplete-provider ollama
contenox config set default-max-tokens 8192
contenox config set default-think high
contenox config set default-chain    .contenox/default-chain.json
contenox config set hitl-policy-name hitl-policy-strict.json

contenox config get default-model
contenox config list
```

Valid global keys: `default-model`, `default-provider`, `default-alt-model`, `default-alt-provider`, `default-autocomplete-model`, `default-autocomplete-provider`, `default-max-tokens`, `default-think`, `telemetry-enabled`, `update-check`, `default-mission-agent`, `default-mission-policy`.

Valid workspace keys: `default-chain`, `hitl-policy-name`.

`default-mission-agent` and `default-mission-policy` are the fallbacks mission mode fires with when none is named — see [the `/mission` slash command](#the-mission-slash-command).

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

> [!NOTE]
> `mcp update --header` and `mcp update --inject` each **replace** the entire corresponding map. Pass all required values in a single update call.

### The `/mission` slash command

Missions are the dual of chat mode. In chat you prompt turn by turn and approve each gated action yourself. In mission mode you fire a one-line intent at a declared agent under an **envelope** — a HITL policy that bounds what it may do unattended — and keep working; the unit acts inside the envelope, and only crossing it costs your attention.

From inside a session (`contenox acp`, or the Beam TUI) fire a mission without leaving the conversation:

- `/mission <intent>` — fires the configured `default-mission-agent` under the `default-mission-policy` envelope.
- `/mission <agent-name> <intent>` — fires the named agent instead.

The two forms are the same shape, so contenox resolves the first token against the declared-agent registry: a hit is the named form, a miss means the whole line is the intent for the default agent. The confirmation always states which agent was chosen and echoes the intent, so a misread is visible immediately.

The dispatch runs **in-process**: the fired unit is a child subprocess of the calling session's own process, no daemon is needed, and the unit's reports stream live back into the firing session as they land. A mission with no agent or no envelope is refused. The hardened `acpx` profile never offers `/mission`.


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
contenox acpx                # headless / untrusted-driver profile
```

The chain each profile loads is overridable via `CONTENOX_ACP_CHAIN_PATH` (acp) and `CONTENOX_ACPX_CHAIN_PATH` (acpx). See the [editor integration guides](/docs/integrations/editors/zed/) for client setup.

### `contenox version`

Prints the current binary version and exits.

```bash
contenox version
```

## Environment variables

| Variable | Description |
|---|---|
| `CONTENOX_ACP_CHAIN_PATH` | Override the chain file used by ACP sessions |
| `CONTENOX_ACPX_CHAIN_PATH`| Override the chain file used by headless ACPX sessions |
| `CONTENOX_DEFAULT_MODEL` / `CONTENOX_DEFAULT_PROVIDER` | Process-level override of the configured default model/provider (nothing is persisted). Also the ACP `env_var` auth-method contract for non-interactive setup. |
| `CONTENOX_DEFAULT_ALT_MODEL` / `CONTENOX_DEFAULT_ALT_PROVIDER` | Same, for the alt model pair. |
| `CONTENOX_DEFAULT_MAX_TOKENS` / `CONTENOX_DEFAULT_THINK` | Same, for the response token cap and reasoning level. |
| `CONTENOX_BASE_URL` | Endpoint URL for account-specific providers whose URL cannot be defaulted (e.g. Vertex: project + region). |
