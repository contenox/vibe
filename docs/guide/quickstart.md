---
title: Quickstart
description: Install Contenox and connect a model.
---

# Quickstart

## 1. Install

**macOS / Linux (one line):**

```bash
curl -fsSL https://contenox.com/install.sh | sh
```

Or download the binary directly from [GitHub Releases](https://github.com/contenox/contenox/releases/latest).

The whole path — install, setup, first prompt — in one take:

![Install demo: install.sh, contenox setup, and a first answer](/install.gif)

---

## 2. Initialize a workspace

Run this once in the project directory you want Contenox to work in:

```bash
contenox init
```

This creates the workspace marker and writes the default chain and HITL policy presets.

---

## 3. Connect a model

For the local path, install [Ollama](https://ollama.com), pull a model, and point Contenox at it:

```bash
ollama pull qwen3:8b
contenox setup          # pick Ollama, done
contenox doctor
```

See the [Ollama guide](/docs/integrations/providers/ollama/) for details, including Ollama Cloud.

Run your first prompt:

```bash
contenox "hello, what can you do?"
```

![contenox backend list showing local and hosted providers, then a first chat on a local model](/quickstart.gif)

Chat is always session-backed — history persists across invocations automatically.
Pass `-e` to compose your message in `$EDITOR` instead of on the command line:

```bash
contenox chat -e
```

---

## 4. Optional editor use

Contenox can also run inside editor or desktop clients that speak ACP. The same chains, model config, tools, and HITL policy are used either way:

- [Use from Zed](/docs/integrations/editors/zed/)
- [Use from JetBrains](/docs/integrations/editors/jetbrains/)
- [Use from AionUi](/docs/integrations/editors/aionui/)
- [Use from OpenClaw](/docs/integrations/editors/openclaw/)

---

## Cloud providers

Contenox needs at least one model to work. Pick the option that fits:

| Option | What you need |
|--------|--------------|
| [Ollama](/docs/integrations/providers/ollama/) | Ollama installed locally, or an Ollama Cloud key |
| [Google Gemini](/docs/integrations/providers/gemini/) | A free Gemini API key (no GPU) |
| [OpenAI](/docs/integrations/providers/openai/) | An OpenAI API key |
| [Anthropic](/docs/integrations/providers/anthropic/) | An Anthropic API key (Claude) |
| [AWS Bedrock](/docs/integrations/providers/bedrock/) | An AWS account with Bedrock model access |

If you're not sure, start with [Ollama](/docs/integrations/providers/ollama/) for a fully local setup, or [Gemini](/docs/integrations/providers/gemini/) for a free hosted key.

---

## Next steps

- **The beam TUI** — run `contenox beam` for chat, plan, and shell in one persistent session
- [**Your first chain**](/docs/guide/first-chain/) — author your own agent in five edits
- [Core concepts](/docs/guide/concepts/) — how chains, tasks, and tools fit together
- [MCP integration](/docs/integrations/tools/mcp/) — connect external tools
