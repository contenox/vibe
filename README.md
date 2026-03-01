# Contenox Vibe

[![Go](https://img.shields.io/badge/Go-1.24+-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

**Vibe** – AI workflows at your fingertips. 

Vibe is a local AI-powered CLI that plans and executes multi-step tasks on your machine — using filesystem and shell tools, driven by your LLM of choice (OpenAI, Gemini, Ollama, vLLM). Zero cloud dependencies required if you run local models. SQLite-backed.

```
$ vibe plan new "install a git pre-commit hook that prevents commits when go build fails"

Creating plan "install-a-git-pre-commit-a3f9e12b" with 5 steps. Now active.
1. [ ] Install necessary tools: Ensure git and go are installed
2. [ ] Create .git/hooks/pre-commit with execute permissions
3. [ ] Edit the hook script with the build check
4. [ ] Write the bash content to the hook file
5. [ ] Make the hook executable: chmod +x .git/hooks/pre-commit

$ vibe plan next --auto

Executing Step 1: Install necessary tools...   ✓
Executing Step 2: Create pre-commit hook...    ✓
Executing Step 3: Edit script content...       ✓
Executing Step 4: Write bash to hook file...   ✓
Executing Step 5: chmod +x .git/hooks/pre-commit  ✓

No pending steps. Plan is complete!
```

```bash
$ cat .git/hooks/pre-commit
#!/bin/bash
set -e
go build ./...
if [ $? -ne 0 ]; then
    echo "Build failed, commit aborted." >&2
    exit 1
fi
```

**The model wrote that.** For real. On your local machine.

---

## What it does

`vibe plan` breaks any natural-language goal into ordered steps, then executes them one at a time using real shell and filesystem tools:

| Tool | What the model can do |
|------|-----------------------|
| `local_shell` | Run bash commands (`bash`, `cat`, `ls`, `chmod`, etc.) |
| `local_fs` | Read and write files on disk |

The plan is persisted to SQLite. You can pause, inspect, retry individual steps, or replan at any point.

---

## Install

```bash
TAG=v0.0.76
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
esac
curl -sL "https://github.com/contenox/vibe/releases/download/${TAG}/vibe-${TAG}-${OS}-${ARCH}" -o vibe
chmod +x vibe
./vibe init
```

Or pick a binary from [Releases](https://github.com/contenox/vibe/releases).

---

## Quick start

### Requirements

- **An LLM Backend**. Any provider works (OpenAI, Gemini, vLLM, Ollama), as long as the model supports tool calling.
  *Example (Local with Ollama)*:
  ```bash
  ollama serve
  ollama pull qwen2.5:7b   # smallest reliable open model for tool use
  ```
  *Example (Cloud)*: Provide your API key in `.contenox/config.yaml` as shown below.

- **Go 1.24+** (to build from source)

### First run

```bash
./vibe init                      # creates .contenox/ in the current directory
./vibe list files in my home directory   # natural language → shell command
```

---

## `vibe plan` — autonomous multi-step execution

`vibe plan` is the power feature: describe a goal once, get back an ordered plan, then execute it step by step.

### Create a plan

```bash
vibe plan new "migrate all TODO comments in the codebase to a TODOS.md"
vibe plan new "set up a git pre-commit hook that blocks commits when go build fails"
vibe plan new "find all .go files larger than 500 lines and write a refactoring report"
```

### Inspect

```bash
vibe plan list          # all plans   (* = active)
vibe plan show          # steps of the active plan with status
```

```
Plan: install-a-git-pre-commit-a3f9e12b (active) — 5/5 complete
1. [x] Install necessary tools
2. [x] Create .git/hooks/pre-commit
3. [x] Edit script content
4. [x] Write bash to hook file
5. [x] chmod +x .git/hooks/pre-commit
```

### Execute

| Command | Behaviour |
|---------|-----------|
| `vibe plan next` | Execute exactly **one** pending step, then pause for review |
| `vibe plan next --auto` | Execute **all** pending steps autonomously |
| `vibe plan retry <N>` | Reset step N back to pending and re-execute |
| `vibe plan skip <N>` | Mark step N skipped and move on |
| `vibe plan replan` | Regenerate remaining steps from current state |
| `vibe plan delete <name>` | Delete a plan by name (removes DB entry and markdown file) |
| `vibe plan clean` | Delete all completed or archived plans |

**Human-in-the-loop is the default** — `next` without `--auto` executes one step and stops. This protects you from runaway automation. Use `--auto` when you trust the plan.

### Full example flow

```bash
# 1. Create
vibe plan new "install a git pre-commit hook that blocks commits when go build fails"

# 2. Review the plan before running anything
vibe plan show

# 3. Execute one step at a time (review each result)
vibe plan next
vibe plan next
vibe plan next
# ...

# Or, once you trust it, run everything at once:
vibe plan next --auto

# 4. If a step went wrong, retry it
vibe plan retry 3

# 5. Check the final state
vibe plan show
```

---

## `vibe` — interactive chat (the classic mode)

```bash
vibe list files in my home directory
vibe --input "what is my current working directory?"
echo "explain this file" | vibe
```

Uses `.contenox/default-chain.json` by default. Natural language → shell commands → results back in chat.

---

## `vibe exec` — run any chain with any input type

For scripting and pipeline use where you want full control over the input type:

```bash
# Default: string input
vibe exec --chain .contenox/my-chain.json "what is the sum of 2+2?"

# Wrap input as a chat message
cat diff.txt | vibe exec --chain .contenox/review.json --input-type chat

# Read input from a file
vibe exec --chain .contenox/doc-chain.json --input @main.go

# Pass structured JSON input
vibe exec --chain .contenox/parse-chain.json --input-type json '{"key":"value"}'
```

`vibe exec` is stateless — no chat history is loaded or saved. `--chain` is required. Supported `--input-type` values: `string` (default), `chat`, `json`, `int`, `float`, `bool`.

---

## Configuration (`.contenox/config.yaml`)

`vibe init` generates this file. Edit it to select your LLM provider.

### Local model (Ollama — default)

```yaml
backends:
  - name: local
    type: ollama
    base_url: http://127.0.0.1:11434
default_provider: local
default_model: qwen2.5:7b    # must support tool calling for vibe plan
context: 32768

enable_local_shell: true
local_shell_allowed_commands: "bash,echo,cat,ls,chmod,sh,date,pwd,head,tail,grep,find,mkdir,cp,mv"
```

### OpenAI

```yaml
backends:
  - name: openai
    type: openai
    base_url: https://api.openai.com/v1
    api_key_from_env: OPENAI_API_KEY   # export OPENAI_API_KEY=sk-...
default_provider: openai
default_model: gpt-4o-mini

enable_local_shell: true
local_shell_allowed_commands: "bash,echo,cat,ls,chmod,sh,date,pwd,head,tail,grep,find,mkdir,cp,mv"
```

### Gemini

```yaml
backends:
  - name: gemini
    type: gemini
    api_key_from_env: GEMINI_API_KEY   # export GEMINI_API_KEY=...
default_provider: gemini
default_model: gemini-2.0-flash

enable_local_shell: true
local_shell_allowed_commands: "bash,echo,cat,ls,chmod,sh,date,pwd,head,tail,grep,find,mkdir,cp,mv"
```

### Other supported backends

| Backend | `type` | Notes |
|---------|--------|-------|
| Ollama  | `ollama` | Local. Run `ollama serve` first. |
| OpenAI  | `openai` | `api_key_from_env` or `api_key` |
| vLLM    | `vllm`   | Self-hosted OpenAI-compatible endpoint |
| Gemini  | `gemini` | `api_key_from_env` or `api_key` |

---

## Build from source

```bash
git clone https://github.com/contenox/vibe
cd vibe
go build ./cmd/vibe/...
./vibe init
```

---

## Architecture

```
vibe CLI
  ├── plan new        → LLM planner chain → SQLite plan + steps
  ├── plan next       → LLM step-executor chain → local_shell / local_fs tools → result persisted
  ├── plan delete     → remove plan from DB + markdown file
  ├── plan clean      → bulk-remove completed/archived plans
  ├── exec            → run any chain, any input type, stateless
  └── run (default)   → LLM chat chain → local_shell / local_fs tools → interactive response

SQLite (.contenox/local.db)
  ├── plans + plan_steps   (vibe plan state)
  ├── message_index        (chat sessions)
  └── kv                   (active session pointer, config)
```

The chains are JSON files in `.contenox/`. They define the LLM workflow: which model to use, which hooks are available, and how to branch based on the model's output.

`vibe run` (the default command) accepts a custom chain via `--chain`:

```bash
vibe --chain .contenox/my-chain.json "summarize this file"
```

The chain's first task must accept `DataTypeChatHistory` as input — meaning its handler must be `chat_completion`. The input (positional args, `--input`, or stdin) is always delivered as a user message in the chat history. Chains producing `DataTypeChatHistory` or `DataTypeString` output print cleanly to stdout; other output types are JSON-marshalled.

The bundled `default-chain.json` gives the model access to `local_shell` and `local_fs` for agentic work. You can write your own chain — with different hooks, different models, or no hooks at all for plain LLM queries.

> **Note:** `vibe plan` uses its own built-in planner and executor chains. `--chain` does not apply to `vibe plan` subcommands.


---

## The Runtime API Server

`vibe` is just the local CLI layer over the core Contenox task engine. 

For full-stack server deployments, the **Runtime API** is an HTTP server exposing the exact same workflow engine backed by PostgreSQL and NATS messaging, deployed via Docker Compose.

- **Docs:** Read the [Server Quickstart](docs/server-quickstart.md).
- **Live Specs:** The `runtime-api` exposes OpenAPI v3. See the live auto-generated API Reference here: [https://contenox.com/docs/openapi.html](https://contenox.com/docs/openapi.html) (Raw [JSON](https://contenox.com/docs/openapi.json) / [YAML](https://contenox.com/docs/openapi.yaml)).

---

> Questions or feedback: **hello@contenox.com**
