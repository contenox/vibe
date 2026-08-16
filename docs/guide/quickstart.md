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

Then confirm the model is ready:

```bash
contenox doctor
```

Its first line is the verdict:

```
Ready: yes
```

If it says `Ready: no`, the line under it names the one command that fixes it.

---

## 3. Initialize a workspace

Run this once in each project directory you want Contenox to work in:

```bash
contenox init
```

This creates the project-local `.contenox/workspace.id` marker and seeds `agents/` and `agents.toml`. The HITL policy presets live in `~/.contenox`, the shipped chains under `~/.contenox/system/`; a workspace-local file with the same name overrides its global counterpart.

---

## 4. Declare an agent

An agent is one file. `.contenox/agents/reviewer.md`:

```markdown
---
name: reviewer
description: Reviews a file for correctness problems
---

You are a code reviewer. Read the file you are asked about, then list the
problems you can point at in what you actually read.
```

No build step — the next run picks it up:

```bash
contenox agent list
contenox mission fire reviewer "review payments.go" --wait
```

The frontmatter says how to run it, the body becomes its system prompt. Budgets, retries and shell allowlists go in [`agents.toml`](/docs/reference/agents-config/) beside it. See [Declaring agents](/docs/guide/agents/).

---

## 5. Optional editor use

Contenox can also run inside editor or desktop clients that speak ACP. The same agents, model config, tools, and HITL policy are used either way:

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

- [**Tutorial: your first agent**](/docs/guide/tutorial-first-agent/) — one file, what contenox builds behind it, and where the knobs are
- [Declaring agents](/docs/guide/agents/) — the full frontmatter, skills, and the tools an agent brings with it
- [Core concepts](/docs/guide/concepts/) — how agents, chains, tasks, and tools fit together
- [Writing a chain by hand](/docs/guide/first-chain/) — for the agent that has outgrown a declaration
- [How contenox compares](/docs/guide/comparison/) — what it shares with the coding agents, and the three things that are built differently
- [MCP integration](/docs/integrations/tools/mcp/) — connect external tools
- [Pairing a machine with a relay](/docs/guide/pairing/) — reach a running session from your phone: one typed key, optional always, free for you and three teammates (one machine each)
- [AI sovereignty & the EU AI Act](/docs/guide/sovereignty/) — hosting, state, and oversight controls you own
