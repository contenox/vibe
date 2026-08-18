---
title: Configuration
description: Backends, defaults, and workspace state — all stored in SQLite, managed with CLI commands.
order: 2
---

# Configuration

Contenox stores all configuration in a single SQLite database at `~/.contenox/local.db`.
There is no YAML file — register backends and set defaults using CLI commands.

## Workspaces vs global

Contenox has two layers of state:

- **Global state** — one shared database at `~/.contenox/local.db`. Holds backends, provider configuration, sessions, MCP registrations, and defaults. Shared by every project on your machine.
- **Global runtime files** — `~/.contenox/` also holds `agents.toml` (which carries the `[envelopes.*]` sections), an `agents/` directory for agents you want everywhere, the transpiled envelopes and compiled chains under `~/.contenox/.generated/`, and the shipped chain files under `~/.contenox/system/`.
- **Workspace state** — one `.contenox/` directory per project, containing a `workspace.id` file (a UUID written on `contenox init`), this project's [agent declarations](/docs/guide/agents/) and `agents.toml`, and any chain or policy files that override a global one by name. Each workspace scopes its own messages and workspace-specific config overrides inside the single global database.

Files resolve by name, workspace first: the workspace `.contenox/`, then `~/.contenox/`, then `~/.contenox/system/`. Copying a shipped chain up out of `system/` is how you take ownership of it.

Running `contenox init` in a project directory creates a `.contenox/` folder with a fresh `workspace.id`, seeds `agents.toml` and `agents/`, and ensures the default runtime files exist under `~/.contenox/`. The same project always resolves to the same workspace regardless of where you invoke `contenox` from, as long as you're inside the directory tree.

Backends and global defaults survive across every workspace. A workspace's sessions and workspace-scoped overrides are invisible to other workspaces.

## Local models

For local inference, run [Ollama](https://ollama.com) (or point at a self-hosted vLLM endpoint) and register it:

```bash
ollama pull qwen3:8b
contenox backend add ollama --type ollama
contenox config set default-provider ollama
contenox config set default-model qwen3:8b
contenox doctor
```

`contenox setup` walks you through the same steps interactively.

## Register cloud or external backends

```bash
# Local Ollama (base URL inferred automatically)
contenox backend add ollama --type ollama

# Ollama Cloud
contenox backend add ollama-cloud --type ollama --url https://ollama.com/api --api-key-env OLLAMA_API_KEY

# OpenAI (base URL inferred)
contenox backend add openai --type openai --api-key-env OPENAI_API_KEY

# Anthropic (base URL inferred)
contenox backend add anthropic --type anthropic --api-key-env ANTHROPIC_API_KEY

# Google Gemini
contenox backend add gemini --type gemini --api-key-env GEMINI_API_KEY

# AWS Bedrock
contenox backend add bedrock --type bedrock --url https://bedrock-runtime.us-east-1.amazonaws.com

# Self-hosted vLLM or compatible endpoint
contenox backend add myvllm --type vllm --url http://gpu-host:8000

# Vertex AI — --url is required (include project and region)
# Option A: service account JSON (works everywhere)
export VERTEX_SA_JSON=$(cat /path/to/service-account.json)
contenox backend add vertex --type vertex-google \
  --url "https://us-central1-aiplatform.googleapis.com/v1/projects/YOUR_PROJECT_ID/locations/us-central1" \
  --api-key-env VERTEX_SA_JSON

# Option B: Application Default Credentials (CLI only)
gcloud auth application-default login
gcloud auth application-default set-quota-project YOUR_PROJECT_ID
contenox backend add vertex --type vertex-google \
  --url "https://us-central1-aiplatform.googleapis.com/v1/projects/YOUR_PROJECT_ID/locations/us-central1"
```

Backends are **global** — they live in `~/.contenox/local.db` and are visible to every workspace.

## Set persistent defaults

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

contenox config list   # review current settings and their scope
```

| Key | Scope | Description |
|---|---|---|
| `default-model` | global | Model name used when `--model` is not passed |
| `default-provider` | global | Provider type used when `--provider` is not passed |
| `default-alt-model` | global | Secondary model exposed to chains through `{{var:alt_model}}` |
| `default-alt-provider` | global | Secondary provider exposed to chains through `{{var:alt_provider}}` |
| `default-autocomplete-model` | global | Model used for editor code-completion (FIM autocomplete) requests over ACP, independent of `default-model` |
| `default-autocomplete-provider` | global | Provider for the autocomplete model, independent of `default-provider` |
| `default-audio-model` | global | Model preferred for requests carrying audio attachments, independent of `default-model`. Unset falls back to `default-model`; audio requests resolve only to audio-capable models either way |
| `default-audio-provider` | global | Provider for the audio model, independent of `default-provider`. Unset uses `default-provider` |
| `default-max-tokens` | global | Optional response token cap exposed through `{{var:max_tokens}}` |
| `default-think` | global | Default reasoning level for supported models (`auto`, `off`, `minimal`, `low`, `medium`, `high`, `xhigh`) |
| `telemetry-enabled` | global | Enable local telemetry logs (`true` / `false`) |
| `update-check` | global | Enable automatic update checks (`true` / `false`) |
| `opt-in-beta` | global | Enable beta features (`true` / `false`; default off): the agent roster (`contenox agent`, user-authored `chain-agent-*` chain discovery), and the [event tier](/docs/guide/events/) (`contenox events`, trigger loading). Off means the features are absent, not disabled. The `CONTENOX_OPT_IN_BETA` environment variable (`1`/`true` on, any other value off) overrides this key for a single invocation |
| `default-mission-agent` | global | Declared agent the ACP `/mission <intent>` slash command falls back to when none is named. `contenox mission fire` always takes the agent as a required argument, so this key does not affect it |
| `default-mission-policy` | global | Envelope (HITL policy) both `/mission` and `contenox mission fire --policy` fall back to when none is named |
| `fleet-max-parallel` | global | Max concurrently open mission units across the fleet (integer; `0` = unlimited; default 8) |
| `log-max-size` | global | Size at which a host log starts a new part, written as a number with an optional unit (`10MB`, `512KB`, `1GB`, or bytes). Applies to [`contenox serve`](/docs/reference/contenox-cli/#contenox-serve-path); default 10MB |
| `log-max-files` | global | How many host log files to keep, counted across every date and part (integer; `0` = unlimited; default 14) |
| `log-max-age-days` | global | Delete host logs whose date is older than this many days (integer; `0` = no age limit; default 14) |
| `default-chain` | workspace | Chain file used in this workspace; falls back to the global value when unset |
| `hitl-policy-name` | workspace | Active HITL policy for this workspace; falls back to the global value when unset. Takes an envelope's filename (`hitl-policy-strict.json`) |

`contenox config list` shows each key's current value **and its scope** (`global` / `workspace`) so you can see whether a setting is inherited or overridden locally.

### Which envelope a surface runs under

`hitl-policy-name` is the persistent setting. Per run, `--hitl-policy` overrides
it on `beam`, `acp`, `acpx` and `serve`, and each surface resolves in three
steps:

1. **`--hitl-policy <name-or-path>`.** A value carrying a path separator is a
   path and is used **verbatim** — that exact file, and a missing one is an error
   rather than a fallback. Anything else is an envelope name; `strict` and
   `hitl-policy-strict.json` name the same one.
2. **Your own file** — a top-level `hitl-policy-<name>.json` in the workspace
   `.contenox/`, then `~/.contenox/`.
3. **The transpiled envelope**, rendered from `[envelopes.<name>]` in
   [`agents.toml`](/docs/reference/agents-config/#envelopesname) into
   `.generated/hitl-policy-<name>.json` on every run.

```bash
contenox beam --hitl-policy strict                       # by envelope name
contenox serve ~/src/api --hitl-policy ./ops/locked.json # by path, verbatim
```

Full detail, including what happens when a name resolves to nothing, is in
[Policy resolution order](/docs/guide/hitl/#policy-resolution-order).

The `default-*` model settings can also be overridden per process — without persisting anything — via the `CONTENOX_DEFAULT_*` environment variables, and `opt-in-beta` via `CONTENOX_OPT_IN_BETA`; see the [environment variables table](/docs/reference/contenox-cli/#environment-variables) in the CLI reference.

## Manage backends

```bash
contenox backend list
contenox backend show openai
contenox backend remove myvllm
```

## Supported providers

| `--type` | Notes                                                                                                     |
| -------- | --------------------------------------------------------------------------------------------------------- |
| `ollama` | Local: run `ollama serve` first. Hosted: use `--url https://ollama.com/api --api-key-env OLLAMA_API_KEY`. |
| `openai` | Use `--api-key-env OPENAI_API_KEY`. Base URL inferred.                                                    |
| `anthropic` | Anthropic Claude (direct API). Use `--api-key-env ANTHROPIC_API_KEY`. Base URL inferred.               |
| `gemini` | Use `--api-key-env GEMINI_API_KEY`. Base URL inferred.                                                    |
| `bedrock` | Amazon Bedrock (Converse API). Requires `--url` (carries the region). Auth: ambient AWS credential chain (env / profile / IAM role), or static keys JSON via `--api-key-env`. |
| `vllm`   | Self-hosted OpenAI-compatible endpoint. Requires `--url`.                                                 |
| `vertex-google` | Vertex AI — Gemini on GCP. Requires `--url` with project and region. Auth: service account JSON via `--api-key-env`, or ADC (no flag needed if `gcloud auth application-default login` is configured). |

## Database location

Contenox uses **one** database at `~/.contenox/local.db` by default. Override with:

- `--db <path>` — use a specific SQLite file (useful for isolated tests or per-environment state)
- `--data-dir <path>` — point at a specific workspace directory (overrides the walk-up discovery)

The walk-up from the current directory only decides **which workspace** you're operating in (by finding a `.contenox/workspace.id` file). The database itself is always the global one unless `--db` is passed.

## External backends for state (opt-in)

By default nothing external is required: the store, the message bus and the key-value cache all live in the one SQLite file above. Three environment variables move them onto servers you run instead. Each is read once, at process start.

| Variable | Moves | Accepted form |
|---|---|---|
| `CONTENOX_POSTGRES_URL` | the store, out of the SQLite file | `postgres://user:pass@host:5432/dbname?sslmode=…`, or a keyword connection string (`host=… user=… dbname=…`) |
| `CONTENOX_NATS_URL` | the message bus, off the database | `nats://host:4222` (also `tls://`, `ws://`, `wss://`); comma-separate a server list |
| `CONTENOX_VALKEY_URL` | the key-value cache, off the database | `valkey://host:6379`, `valkey://user:password@host:6379`, `valkey://:password@host:6379/3?namespace=contenox`, or a bare `host:6379` |

Unset means the SQLite file, unchanged — no migration, no prompt, no difference in behaviour.

Rules worth knowing before you set any of them:

- **A setting that cannot be used stops the process.** A malformed value, or a server that will not accept a connection, is reported by name at startup and the command exits. Contenox never quietly falls back to the local file: asking for Postgres and getting a SQLite file would leave you reading the wrong state.
- **`CONTENOX_POSTGRES_URL` requires the other two.** The database-backed bus and key-value table are written for SQLite and are refused by Postgres, so selecting Postgres without `CONTENOX_NATS_URL` and `CONTENOX_VALKEY_URL` is rejected rather than half-wired.
- **Terminate TLS in front of Valkey.** A `valkeys://` or `rediss://` URL is refused instead of being silently downgraded to a plaintext connection.
- **The schema is applied on connect**, exactly as it is for the SQLite file, so an empty Postgres database is enough to start against.
- **Every process reads the same variables.** Export them where the CLI and any surface you launch will inherit them, or each will resolve its own state.
- **A shared NATS server is shared state.** Subject names are fixed (`mcp.*`, `missionservice.events.*`, and the rest), with nothing in them that identifies a deployment. Two contenox deployments pointed at one NATS server therefore see each other's requests and events. Give each its own server, or its own NATS account.

### Isolating contenox inside a Valkey you already run

Three parts of `CONTENOX_VALKEY_URL` keep the cache out of the way of whatever else uses that server, and all three are honoured or refused — never dropped:

- **The user.** `valkey://appuser:secret@cache:6379` authenticates as `appuser`. A URL with only a password (`valkey://:secret@cache:6379`) authenticates as the server's default user, as before. A user with no password (`valkey://appuser@cache:6379`) is refused rather than sent: unlike NATS, Valkey has nowhere to put a token in the user position.
- **The database index.** The path is the database to `SELECT`: `valkey://cache:6379/3` uses database 3. No path means database 0. A path that is not a database index — `/contenox`, `/-1`, `/3/extra` — is refused at startup rather than quietly becoming 0. A server that will not honour the index (a cluster, or one configured with fewer databases) fails the connection, and the command stops with the variable named.
- **The key namespace.** `?namespace=contenox` prefixes every key contenox writes with `contenox:`, so its `prov:*` and `presence:*` keys become `contenox:prov:*` and `contenox:presence:*` and cannot collide with another tenant's. The prefix is invisible to contenox itself. It also gives you something to write an ACL rule against: `ACL SETUSER appuser on >secret ~contenox:* +@all` confines contenox to its own keys.

A namespace is a literal prefix, so it cannot contain whitespace or the glob characters `*?[]\`. `namespace` is the only query parameter read; any other — `?db=3` included, since the database index belongs in the path — is refused rather than ignored.

Every process that shares the cache must be given the same database index and namespace: they are part of the address, not a per-process preference.

### Checking which backend a process actually uses

[`contenox doctor`](/docs/reference/contenox-cli/#contenox-doctor) grows a **State storage** section as soon as one of the three is set. It names the backend behind each of them and, for a remote one, whether it answered:

```
State storage:
  • store: Postgres (postgres://contenox:xxxxx@db:5432/contenox, from CONTENOX_POSTGRES_URL)
    Status: reachable
  • message bus: NATS (nats://bus:4222, from CONTENOX_NATS_URL)
    Status: reachable
  • key-value cache: SQLite (/home/you/.contenox/local.db)
    Status: local file
```

A Valkey line prints the URL you set rather than just its host — `key-value cache: Valkey (valkey://appuser:xxxxx@cache:6379/3?namespace=contenox, from CONTENOX_VALKEY_URL)` — so the database index and namespace a process is actually using are visible where you check them.

Credentials are masked there, in the URL form and in a keyword connection string. A URL that carries no password loses its whole userinfo instead of the password alone — token auth puts the credential where a username goes, as `nats://<token>@host:4222` does — so a bare `postgres://contenox@db/…` prints as `postgres://xxxxx@db/…` too. With none of the three set the section is absent, which is itself the answer: everything is in the SQLite file. A remote backend that does not answer is named with the variable that selected it, and the command then stops rather than reporting on a runtime it cannot build.

### What opting in gets you

Moving state off the file is worth it when it has to outlive the machine — a container host with no durable disk — or when it belongs in infrastructure you already operate: a Postgres you back up, monitor and can query with your own tools; a NATS server a deployment already runs; a Valkey already in place. Nothing else changes: the same commands, the same schema, the same [event log](/docs/guide/events/), and the same workspace layout on disk.

### What this does not claim

**Selecting shared backends does not make several contenox processes safe to run against one of them.** Nothing here coordinates two runtimes: there is no leader election, no distributed lock, and no fencing token. Each process opens the backends named in its own environment, runs its own background passes, and treats the state it reads as its own. Individual mechanisms do claim a row before working on it, but that is not the same as a deployment designed — or tested — for two processes sharing one backend.

Run one contenox process per set of backends. A multi-process deployment against shared state is not something contenox supports today, and pointing these variables at a shared server does not create it.

Two more things these settings are not:

- **Not a migration.** An existing SQLite file is not copied, read, or converted. Selecting Postgres starts against whatever is in that database — an empty one comes up empty, with no backends and no sessions, and `contenox doctor` will say so.
- **Not a fallback pair.** With a variable set there is one backend, not a preferred one and a spare. If the server is unreachable the command fails instead of continuing on the local file.
