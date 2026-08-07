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

## 2. Connect a model

`contenox setup` is the entry point. For the local path, install [Ollama](https://ollama.com) and pull a model first:

```bash
ollama pull qwen3:8b
contenox setup          # pick Ollama, then pick your model from the list
```

When Ollama is running, the wizard reads the models you have actually pulled and offers them as a numbered menu — press Enter to take the suggested one.

See the [Ollama guide](/docs/integrations/providers/ollama/) for details, including Ollama Cloud.

Then confirm you can chat:

```bash
contenox doctor
```

Its first line is the verdict:

```
Ready: yes — chat now with `contenox new` or `contenox "your prompt"`.
```

If it says `Ready: no`, the line under it names the one command that fixes it.

---

## 3. Initialize a workspace

Run this once in each project directory you want Contenox to work in:

```bash
contenox init
```

This creates the project-local `.contenox/workspace.id` marker; the default chains and HITL policy presets live globally in `~/.contenox` (a workspace-local file with the same name overrides its global counterpart). See [Your first chain](/docs/guide/first-chain/) for the full layout.

---

## 4. Start working

The terminal UI is the main surface — chat, plan, and shell in one persistent session:

```bash
contenox new
```

The transcript flows into your terminal's own scrollback, `/` opens commands, `!` runs a shell line, `@` attaches a file, and gated tool calls are answered inline with one keystroke. Press `?` on an empty composer for the full key list.

`new` always starts fresh. To carry on where you left off, reopen the last session with its transcript replayed:

```bash
contenox resume
```

For one-shot and scripted use, pass the prompt directly:

```bash
contenox "hello, what can you do?"
echo "summarise README.md" | contenox
```

![contenox backend list showing local and hosted providers, then a first chat on a local model](/quickstart.gif)

Chat is always session-backed — history persists across invocations automatically.
Pass `-e` to compose your message in `$EDITOR` instead of on the command line:

```bash
contenox chat -e
```

---

## 5. Optional editor use

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
| [Vertex AI](/docs/integrations/providers/vertex/) | Gemini billed through your GCP project |
| [vLLM / OpenAI-compatible](/docs/integrations/providers/openai/) | Any server speaking the OpenAI API (vLLM, LM Studio, …) |

If you're not sure, start with [Ollama](/docs/integrations/providers/ollama/) for a fully local setup, or [Gemini](/docs/integrations/providers/gemini/) for a free hosted key.

---

## Next steps

- [**Your first chain**](/docs/guide/first-chain/) — author your own agent in five edits
- [Core concepts](/docs/guide/concepts/) — how chains, tasks, and tools fit together
- [How contenox compares](/docs/guide/comparison/) — what it shares with the coding agents, and the three things that are built differently
- [MCP integration](/docs/integrations/tools/mcp/) — connect external tools
- [Workspace index & search](/docs/guide/search/) — ask the repo a question, get file:line citations back
- [AI sovereignty & the EU AI Act](/docs/guide/sovereignty/) — hosting, state, and oversight controls you own
