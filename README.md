# Contenox

[![Go](https://img.shields.io/badge/Go-1.24+-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Version](https://img.shields.io/github/v/release/contenox/contenox?label=version&logo=github)](https://github.com/contenox/contenox/releases)

**Built so your workflow stays on your side.**

Most AI tools make your workflow dependent on systems you don't control. When they fail, reset, or change — you start over. Contenox keeps your context, executes real work, and routes to whichever model fits the job — so you're not renting your ability to ship.

Not another chat. A system that executes.

📖 **[contenox.com](https://contenox.com)**

---

## Install

<!-- Release tooling: keep next line in sync with apiframework/version.txt (updated by `make -f Makefile.version bump-*`). -->
<!-- TAG=v0.9.1 -->

```bash
curl -fsSL https://contenox.com/install.sh | sh
```

---

## What it is

Contenox is an AI copilot you own. Any model, any provider — the workflow stays yours.

```bash
contenox plan new "install a git pre-commit hook that prevents commits when go build fails"
contenox plan next --auto
```

```
Executing Step 1: Install necessary tools...              ✓
Executing Step 2: Create .git/hooks/pre-commit...         ✓
Executing Step 3: Edit the hook script with the check...  ✓
Executing Step 4: Write bash content to the hook file...  ✓
Executing Step 5: chmod +x .git/hooks/pre-commit...       ✓
```

Plans are stored in SQLite. They don't evaporate when the session ends.

---

## Desktop app

```bash
contenox
```

Opens the Contenox desktop app. Terminal output, file reads, live agent steps, and plan state appear inline in the same thread you're typing in. No cloud required.

---

## Plans that stick

```bash
contenox plan new "migrate all TODO comments to TODOS.md"
contenox plan show         # review before touching anything
contenox plan next         # one step at a time
contenox plan next --auto  # or all at once
contenox plan retry 3      # step went wrong? retry it
```

---

## Connect your stack

```bash
# Any MCP-compatible tool
contenox mcp add filesystem --transport stdio \
  --command npx --args "-y,@modelcontextprotocol/server-filesystem,/"

# Any HTTP API
contenox hook add my-api --url http://localhost:8000
contenox run "fetch the latest metrics and summarize them"
```

---

## Backends

Switch providers in one config line:

```bash
contenox backend add local   --type ollama
contenox backend add openai  --type openai  --api-key-env OPENAI_API_KEY
contenox backend add gemini  --type gemini  --api-key-env GEMINI_API_KEY

contenox config set default-model qwen2.5:7b
contenox config set default-provider ollama
```

---

## Build from source

```bash
git clone https://github.com/contenox/contenox
cd contenox
make build-contenox
```

---

> Questions: **hello@contenox.com**
