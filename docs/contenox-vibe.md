# Vibe

**Vibe** is the local CLI layer over the Contenox task engine. It runs without Postgres, NATS, or a tokenizer service — just SQLite and an in-memory bus. Point it at Ollama, OpenAI, vLLM, or Gemini, and run AI workflows from the terminal: interactive chat, multi-step autonomous plans, or arbitrary chain pipelines.

---

## Quick start

```bash
# From a release binary:
./vibe init                          # scaffold .contenox/ with config + default chain
./vibe "list files in my home dir"   # natural language → shell → response

# Or build from source:
git clone https://github.com/contenox/vibe.git
cd vibe
go build -o vibe ./cmd/vibe/...
./vibe init
```

**Requirements:** Ollama running (`ollama serve`) and a model that supports tool calling:
```bash
ollama pull qwen2.5:7b
```

---

## Subcommands

### `vibe` / `vibe run` — interactive chat

```bash
vibe "what is the current directory?"
vibe --input "explain this error" < build.log
echo "summarise this" | vibe
```

Input comes from positional args, `--input`, or stdin (in that order of priority). The result is printed to stdout. Uses `.contenox/default-chain.json` by default; override with `--chain`.

---

### `vibe plan` — autonomous multi-step execution

Break a goal into an ordered plan of steps, then execute them one at a time (or all at once). State is persisted in SQLite so you can pause, inspect, retry, or replan at any point.

```bash
# Create a plan
vibe plan new "set up a git pre-commit hook that blocks commits when go build fails"

# Inspect
vibe plan list          # all plans  (* = active)
vibe plan show          # steps of the active plan

# Execute
vibe plan next          # run one step, then stop
vibe plan next --auto   # run all pending steps

# Control
vibe plan retry <N>     # reset step N to pending and re-run
vibe plan skip <N>      # mark step N skipped
vibe plan replan        # regenerate remaining steps from current state

# Cleanup
vibe plan delete <name> # remove a plan (DB + .contenox/plans/<name>.md)
vibe plan clean         # remove all completed and archived plans
```

Plan names are derived from the goal text (`fix-auth-token-expiry-a3f9e12b`), so they're readable in `plan list` and in the markdown snapshot written to `.contenox/plans/`.

> **Human-in-the-loop by default.** `vibe plan next` executes exactly one step and stops. Use `--auto` only when you trust the plan.

---

### `vibe exec` — run any chain, any input type

For scripting and pipeline use cases where you want full control:

```bash
# String input (default)
vibe exec --chain .contenox/my-chain.json "is this code safe?"

# Wrap as a chat message
cat diff.txt | vibe exec --chain .contenox/review.json --input-type chat

# Read input from a file
vibe exec --chain .contenox/doc-chain.json --input @main.go

# Structured JSON input
vibe exec --chain .contenox/parse.json --input-type json '{"key":"value"}'
```

`--chain` is required. Supported `--input-type` values: `string` (default), `chat`, `json`, `int`, `float`, `bool`.

`vibe exec` is **stateless** — no session history is loaded or saved.

---

### `vibe hook` — manage remote hooks

Register external HTTP services as LLM tools. The runtime fetches the service's `/openapi.json`, discovers every operation, and exposes them as callable tools in chains.

**Real example: US National Weather Service** — free, no API key, OpenAPI spec at `https://api.weather.gov/openapi.json`.

```bash
# Register
vibe hook add nws --url https://api.weather.gov --timeout 15000

# Inspect — lists all discovered tools live from the schema
vibe hook show nws
# Name:    nws
# URL:     https://api.weather.gov
# Timeout: 15000ms
# Tools (60):
#   point                    Returns metadata about a given latitude/longitude point
#   alerts_active_area       Returns active alerts for the given area (state or marine area)
#   alerts_active_count      Returns info on the number of active alerts
#   gridpoint_forecast       Returns a textual forecast for a 2.5km grid area
#   ...
```

Run a query using the included example chain:
```bash
vibe exec --chain .contenox/chain-nws.json --input-type chat \
  "how many active weather alerts are there right now?"
```

Manage hooks:
```bash
vibe hook list                                    # NAME  URL  TIMEOUT
vibe hook update nws --timeout 30000              # update timeout
vibe hook update nws --header "X-App: myapp"      # add a header
vibe hook remove nws                              # remove
```

**Use in any chain** — reference by name in `execute_config.hooks`:
```json
"execute_config": {
  "model": "qwen2.5:7b",
  "provider": "ollama",
  "hooks": ["nws"]
}
```

Header values are never echoed back (`hook show` prints header keys only). If the service is unreachable at registration time, the hook is still saved and validated at execution time.

> **NWS note:** Forecast lookups require two calls — the model first calls `point` with lat/lon to get the grid reference, then `gridpoint_forecast` with that reference. The included `chain-nws.json` explains this in its system prompt.

---

## Configuration (`.contenox/config.yaml`)

`vibe init` generates a starter config. All keys are optional; CLI flags override config.

```yaml
# Minimal Ollama setup
backends:
  - name: local
    type: ollama
    base_url: http://127.0.0.1:11434
default_provider: local
default_model: qwen2.5:7b
context: 32768

enable_local_shell: true
local_shell_allowed_commands: "bash,echo,cat,ls,chmod,sh,date,pwd,head,tail,grep,find,mkdir,cp,mv"
```

### Supported backends

| Key `type` | Provider | Notes |
|------------|----------|-------|
| `ollama`   | Ollama  | Local. Run `ollama serve` first. |
| `openai`   | OpenAI  | `api_key_from_env: OPENAI_API_KEY` |
| `vllm`     | vLLM    | Self-hosted OpenAI-compatible endpoint |
| `gemini`   | Gemini  | `api_key_from_env: GEMINI_API_KEY` |

### Config keys reference

| Key | CLI flag | Purpose |
|-----|----------|---------|
| `default_chain` | `--chain` | Path to chain JSON (relative to `.contenox/`) |
| `db` | `--db` | SQLite path (default: `.contenox/local.db`) |
| `default_provider` | `--model` | Provider name from `backends` |
| `default_model` | `--model` | Model name |
| `context` | `--context` | Context length in tokens |
| `enable_local_shell` | `--enable-local-exec` | Enable `local_shell` hook |
| `local_shell_allowed_commands` | `--local-exec-allowed-commands` | Comma-separated allow list |
| `local_shell_allowed_dir` | `--local-exec-allowed-dir` | Directory scope for allowed executables |
| `local_shell_denied_commands` | `--local-exec-denied-commands` | Block list (checked first) |
| `tracing` | `--trace` | Emit structured operation telemetry to stderr |
| `steps` | `--steps` | Print execution steps after result |
| `raw` | `--raw` | Print full output instead of last assistant message |
| `template_vars_from_env` | — | List of env var names to expose as `{{var:NAME}}` in chains |

---

## The `local_shell` hook

Runs commands on your local machine — real side effects. **Opt-in only.**

Enable with `enable_local_shell: true` in config or `--enable-local-exec` flag. You must also set an allow list or no commands will run.

**Security controls:**
- `local_shell_allowed_commands` — comma-separated list of allowed executable names/paths (at least one required)
- `local_shell_allowed_dir` — only run binaries/scripts under this directory
- `local_shell_denied_commands` — block list checked before the allow list

The model receives the tool schema via the API call protocol. When local_shell is disabled the hook is simply not registered — chains that reference it will run without it.

---

## Output and flags

| Flag | Effect |
|------|--------|
| *(default)* | Quiet: "Thinking…" on stderr while running, result on stdout |
| `--trace` | Structured operation telemetry on stderr (op_id, duration, model selected, etc.) |
| `--steps` | Print task list with handler and duration after the result |
| `--raw` | Print the full output value (e.g. full chat history JSON) |

---

## Chains

Chains are JSON files that define the LLM workflow: which model, which hooks, how to branch based on output. Place them in `.contenox/` and reference by path.

### Macros in chains

Chain fields like `system_instruction` and `prompt_template` support macros expanded before execution:

| Macro | Expands to |
|-------|-----------|
| `{{var:model}}` | Current model name |
| `{{var:provider}}` | Current provider name |
| `{{var:chain}}` | Chain ID |
| `{{var:NAME}}` | Value from `template_vars_from_env` config (vibe only) |
| `{{now}}` / `{{now:layout}}` | Current time |
| `{{chain:id}}` | Chain ID (same as `{{var:chain}}`) |
| `{{hookservice:list}}` | All registered hooks + tools as JSON (useful for inspection/debug chains; the model already receives per-task tool schemas via the API protocol) |
| `{{hookservice:hooks}}` | Hook names only |
| `{{hookservice:tools <hook>}}` | Tool names for a specific hook |

### `--chain` and `vibe plan`

`--chain` selects which chain `vibe run`/`vibe exec` uses. It does **not** apply to `vibe plan` subcommands — the planner and executor chains for `vibe plan` are built-in and live in `.contenox/chain-planner.json` and `.contenox/chain-executor.json` (written by `vibe init`). These chains have a specific contract (input/output types, handler sequence) and are validated on use.

---

## Build from source

```bash
git clone https://github.com/contenox/vibe.git
cd vibe
go build -o vibe ./cmd/vibe/...
./vibe init
```
