# Contenox

[![Go](https://img.shields.io/badge/Go-1.24+-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![GitHub release](https://img.shields.io/github/release/contenox/contenox.svg)](https://github.com/contenox/contenox/releases)

**AI workflows at your fingertips**

Contenox is a lightning-fast, fully-local CLI that turns natural language goals into **persistent, step-by-step plans** and executes them with real shell + custom hooks like filesystem tools. Powered by any LLM (Ollama, OpenAI, Gemini, vLLM, etc.). Zero cloud required.

```bash
$ contenox plan new "install a git pre-commit hook that prevents commits when go build fails"
Creating plan "install-a-git-pre-commit-a3f9e12b" with 5 steps. Now active.

$ contenox plan next --auto
Executing Step 1: Install necessary tools...              ✓
Executing Step 2: Create .git/hooks/pre-commit...         ✓
Executing Step 3: Edit the hook script with the check...  ✓
Executing Step 4: Write bash content to the hook file...  ✓
Executing Step 5: chmod +x .git/hooks/pre-commit...       ✓

No pending steps. Plan is complete!
```

**The model wrote that hook.** On *your* machine. No copy-paste hell.

---

⭐ Leave us a star if you like it! | 🌟 We welcome any suggestions, and contributions!

---

### 📺 `contenox vibe` — Interactive TUI

When you want more than a shell prompt:

```bash
contenox vibe
```

A full-screen terminal dashboard (Bubble Tea) with:
- **Live plan sidebar** — watch steps execute with `⟳` / `✓` / `✗` indicators in real time
- **Interactive approvals** — approve or deny sensitive filesystem actions with `y`/`n` before they run
- **Full CLI parity** — every `contenox` subcommand is a slash command inside vibe: `/plan`, `/model`, `/session`, `/backend`, `/hook`, `/mcp`, `/config`, `/run`

```
/plan new "add prometheus metrics to the HTTP server"
/plan next --auto              ← run to completion
/model set-context gpt-5-mini --context 128k
/backend add local --type ollama --url http://127.0.0.1:11434
/mcp add memory --transport stdio --command npx --args "-y,@modelcontextprotocol/server-memory"
/help
```

---

## Why Contenox?

Contenox is different:

- **Persistent plans** stored in SQLite — pause, inspect, retry, replan at any time
- **Human-in-the-loop by default** — `--auto` only when you say so
- **Real tools** — shell commands and filesystem, not just code suggestions
- **Fully offline** with Ollama — no data leaves your machine
- **Chains are just JSON** — write your own LLM workflows
- **Workflow Engine** — Contenox is not a toy, a complete statemachine lives under the hood.
- **Native MCP Support** — connect to local filesystems, memory servers, and remote tools instantly via the Model Context Protocol.

---

## 🔌 Universal Tooling with MCP

Contenox is a native [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) client. Instead of writing custom integrations, you can instantly connect your local agent to any MCP-compatible data source, persistent memory, or tool API.

```bash
# Give your agent access to the local filesystem
contenox mcp add filesystem --transport stdio \
  --command npx --args "-y,@modelcontextprotocol/server-filesystem,/"

# Give your agent a persistent memory graph across reboots
contenox mcp add memory --transport stdio \
  --command npx --args "-y,@modelcontextprotocol/server-memory"

# Connect to cloud tools securely over SSE
contenox mcp add cloud-tools --transport sse --url https://api.example.com/mcp
```

Every registered MCP server becomes natively available to your agent during chat sessions and execution plans.

---

## 🛠 Turn Any API into an Agent Tool

Don't need the MCP ecosystem? Expose any HTTP API as an agent tool in seconds with `contenox hook add`.
Write a [FastAPI](https://fastapi.tiangolo.com/) service — Contenox reads its OpenAPI schema and makes every endpoint callable by the model, with no extra glue code.

```bash
# Register your FastAPI service as a tool
contenox hook add my-api --url http://localhost:8000

# The model can now call any endpoint on it directly as a tool
contenox run "fetch the latest metrics from my API and summarize them"
```

Any service that speaks HTTP and exposes an OpenAPI spec becomes a first-class agent tool.

---

## Quick Start

### Install

**Ubuntu / Linux**
```bash
TAG=v0.6.1
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
curl -sL "https://github.com/contenox/contenox/releases/download/${TAG}/contenox-${TAG}-linux-${ARCH}" -o contenox
chmod +x contenox && sudo mv contenox /usr/local/bin/contenox
contenox --version
```

**macOS**
```bash
TAG=v0.6.1
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/arm64/arm64/')
curl -sL "https://github.com/contenox/contenox/releases/download/${TAG}/contenox-${TAG}-darwin-${ARCH}" -o contenox
chmod +x contenox && sudo mv contenox /usr/local/bin/contenox
contenox --version
```

Or pick a binary from [Releases](https://github.com/contenox/contenox/releases).

### First run

```bash
# 1. Initialize (creates .contenox/ with default chains)
contenox init

# 2. Register a backend
ollama serve && ollama pull qwen2.5:7b
contenox backend add local --type ollama
contenox config set default-model qwen2.5:7b

# Or for OpenAI / Gemini:
# contenox backend add openai --type openai --api-key-env OPENAI_API_KEY
# contenox config set default-model gpt-5-mini

# 3. Chat with your model:
contenox "hey, what can you do?"
echo 'fix the typos in README.md' | contenox

# 4. Plan and execute a multi-step task:
contenox plan new "create a TODOS.md from all TODO comments in the codebase"
contenox plan next --auto
```

**Requirements:** an LLM with tool calling support.
*Local:* `ollama serve && ollama pull qwen2.5:7b`
*Cloud:* register a backend with `contenox backend add` and set your API key via `--api-key-env`.

---

### Full example

```bash
# 1. Create
contenox plan new "install a git pre-commit hook that blocks commits when go build fails"

# 2. Review the plan before touching anything
contenox plan show

# 3. Execute one step at a time
contenox plan next
contenox plan next
# ...

# Or run everything at once once you trust it
contenox plan next --auto

# 4. If a step went wrong
contenox plan retry 3

# 5. Final check
contenox plan show
```

---

## `contenox plan` — AI-driven plans

```bash
contenox plan new "migrate all TODO comments in the codebase to TODOS.md"
contenox plan new "set up a git pre-commit hook that blocks commits when go build fails"
contenox plan new "find all .go files larger than 500 lines and write a refactoring report"
```

Contenox breaks any goal into an ordered plan, then executes it step by step using real tools.

### Commands

| Command | What it does |
|---|---|
| `contenox plan next` | Run **one** step (safe default — review before continuing) |
| `contenox plan next --auto` | Run **all** remaining steps autonomously |
| `contenox plan show` | See the active plan + step status |
| `contenox plan list` | All plans (`*` = active) |
| `contenox plan retry <N>` | Re-run a failed step |
| `contenox plan skip <N>` | Mark a step skipped and move on |
| `contenox plan replan` | Let the model rewrite the remaining steps |
| `contenox plan delete <name>` | Delete a plan by name |
| `contenox plan clean` | Delete all completed plans |

**Pro tip:** Always do `contenox plan show` before `--auto`.

---

### `contenox chat` — Persistent chat session

```bash
contenox chat "what is my current working directory?"
contenox chat "list files in my home directory"
echo "explain this" | contenox chat
```

Uses `.contenox/default-chain.json`. Natural language → shell tools → response.

### `contenox run` — Scriptable, stateless execution

For CI/pipelines where you want full control:

```bash
contenox run --chain .contenox/my-chain.json "what is 2+2?"
cat diff.txt | contenox run --chain .contenox/review.json --input-type chat
contenox run --chain .contenox/doc-chain.json --input @main.go
contenox run --chain .contenox/parse-chain.json --input-type json '{"key":"value"}'
```

`run` is stateless — no chat history. `--chain` is required. Supported `--input-type`: `string` (default), `chat`, `json`, `int`, `float`, `bool`.

### 🧠 Reasoning model support

Pass `--think` to stream the model's internal chain-of-thought to stderr before it acts — works with DeepSeek-R1, OpenAI o3, Gemini Thinking, and Ollama thinking models:

```bash
contenox --think "why is my API slow?"
contenox run --chain .contenox/review.json --think --input @main.go
```

---

## Configuration

Contenox stores all configuration in SQLite (`.contenox/local.db` or `~/.contenox/local.db`).
No YAML file needed — register backends and set defaults using CLI commands.

### Register a backend

```bash
# Local Ollama (URL inferred automatically)
contenox backend add local --type ollama

# OpenAI (base URL inferred)
contenox backend add openai --type openai --api-key-env OPENAI_API_KEY

# Google Gemini
contenox backend add gemini --type gemini --api-key-env GEMINI_API_KEY

# Self-hosted vLLM or compatible endpoint
contenox backend add myvllm --type vllm --url http://gpu-host:8000
```

### Set persistent defaults

```bash
contenox config set default-model    qwen2.5:7b
contenox config set default-provider ollama
contenox config set default-chain    .contenox/default-chain.json

contenox config list   # review current settings
```

### Manage backends

```bash
contenox backend list
contenox backend show openai
contenox backend remove myvllm
```

| Backend | `--type` | Notes |
|---|---|---|
| Ollama | `ollama` | Local. `ollama serve` first. |
| OpenAI | `openai` | Use `--api-key-env OPENAI_API_KEY` |
| Gemini | `gemini` | Use `--api-key-env GEMINI_API_KEY` |
| vLLM   | `vllm`   | Self-hosted OpenAI-compatible endpoint |

---

## Safety

- **Opt-in shell access** — `--shell` flag must be passed explicitly to enable local_shell
- **Chain-scoped policy** — allowed and denied commands are declared in the chain's `hook_policies` field; the default chains ship with a sensible allowlist out of the box
- **Human-in-the-loop** — `plan next` executes one step and stops; `--auto` requires explicit intent
- **Local-first** — with Ollama, nothing leaves your machine

---

## Architecture

```
contenox CLI
  ├── plan new       → LLM planner chain → SQLite plan + steps
  ├── plan next      → LLM executor chain → local_shell / local_fs → result persisted
  ├── vibe           → Bubble Tea TUI: chat + live plan sidebar + HITL approvals
  ├── run            → run any chain, any input type, stateless
  ├── (bare)         → stateless run via default-run-chain.json (same as run)
  └── chat           → LLM chat chain → session history persisted in SQLite

SQLite (.contenox/local.db)
  ├── plans + plan_steps   (autonomous plan state)
  ├── message_index        (chat sessions)
  └── kv                   (active session + config)
```

Chains are JSON files in `.contenox/`. They define the LLM workflow: model, hooks, branching logic. See [ARCHITECTURE.md](ARCHITECTURE.md) for the full picture.

Contenox is powered by a battle-tested enterprise workflow engine. The [Runtime API](docs/server-quickstart.md) is also available as a self-hostable Docker deployment for teams who want the full server with REST API, observability, and multi-tenant support.

---

## Build from source

```bash
git clone https://github.com/contenox/contenox
cd contenox
go build -o contenox ./cmd/contenox
contenox init
```

---

> Questions or feedback: **hello@contenox.com**
