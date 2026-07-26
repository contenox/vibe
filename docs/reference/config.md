# Configuration

Contenox stores all configuration in a single SQLite database at `~/.contenox/local.db`.
There is no YAML file — register backends and set defaults using CLI commands.

## Workspaces vs global

Contenox has two layers of state:

- **Global state** — one shared database at `~/.contenox/local.db`. Holds backends, provider configuration, sessions, MCP registrations, and defaults. Shared by every project on your machine.
- **Global runtime files** — `~/.contenox/` also stores the shipped default chain/HITL policy presets.
- **Workspace state** — one `.contenox/` directory per project, containing a `workspace.id` file (a UUID written on `contenox init`) and optional workspace chain overrides. Each workspace scopes its own messages and workspace-specific config overrides inside the single global database.

Running `contenox init` in a project directory creates a `.contenox/` folder with a fresh `workspace.id` and ensures the default runtime files exist under `~/.contenox/`. The same project always resolves to the same workspace regardless of where you invoke `contenox` from, as long as you're inside the directory tree.

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
contenox config set default-alt-model gemini-2.5-flash
contenox config set default-alt-provider gemini
contenox config set default-autocomplete-model qwen2.5-coder:7b
contenox config set default-autocomplete-provider ollama
contenox config set default-max-tokens 8192
contenox config set default-think high
contenox config set default-chain    .contenox/default-chain.json
contenox config set hitl-policy-name hitl-policy-strict.json

contenox config list   # review current settings and their scope
```

| Key | Scope | Description |
|---|---|---|
| `default-model` | global | Model name used when `--model` is not passed |
| `default-provider` | global | Provider type used when `--provider` is not passed |
| `default-alt-model` | global | Secondary model exposed to chains through `{{var:alt_model}}` |
| `default-alt-provider` | global | Secondary provider exposed to chains through `{{var:alt_provider}}` |
| `default-autocomplete-model` | global | Optional autocomplete model exposed to chains through `{{var:autocomplete_model}}` |
| `default-autocomplete-provider` | global | Optional autocomplete provider exposed to chains through `{{var:autocomplete_provider}}` |
| `default-max-tokens` | global | Optional response token cap exposed through `{{var:max_tokens}}` |
| `default-think` | global | Default reasoning level for supported models (`auto`, `off`, `minimal`, `low`, `medium`, `high`, `xhigh`) |
| `telemetry-enabled` | global | Enable local telemetry logs (`true` / `false`) |
| `update-check` | global | Enable automatic update checks (`true` / `false`) |
| `default-mission-agent` | global | Declared agent `/mission` and `mission fire` fall back to when none is named |
| `default-mission-policy` | global | Envelope (HITL policy) `/mission` and `mission fire` fall back to when none is named |
| `default-chain` | workspace | Chain file used in this workspace; falls back to the global value when unset |
| `hitl-policy-name` | workspace | Active HITL policy for this workspace; falls back to the global value when unset |

`contenox config list` shows each key's current value **and its scope** (`global` / `workspace`) so you can see whether a setting is inherited or overridden locally.

The `default-*` model settings can also be overridden per process — without persisting anything — via the `CONTENOX_DEFAULT_*` environment variables; see the [environment variables table](/docs/reference/contenox-cli/#environment-variables) in the CLI reference.

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
