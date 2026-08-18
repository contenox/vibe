---
title: Quickstart
description: Install contenox, connect a model, declare an agent, and start working in the terminal with contenox beam.
order: 1
---

# Quickstart

Install, connect a model, declare an agent, and talk to it. Five steps, and the
last one is the one that pays.

## 1. Install

**macOS / Linux (one line):**

```bash
curl -fsSL https://contenox.com/install.sh | sh
```

Or download the binary directly from [GitHub Releases](https://github.com/contenox/contenox/releases/latest).

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

Its first line is the verdict, and it names what to run next:

```
Ready: yes — run: contenox beam
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
```

The frontmatter says how to run it, the body becomes its system prompt. Budgets, retries and shell allowlists go in [`agents.toml`](/docs/reference/agents-config/) beside it. See [Declaring agents](/docs/guide/agents/).

---

## 5. Start working

```bash
contenox beam
```

That is the front door. `contenox` on its own opens the same thing.

The transcript is your native terminal scrollback, so it scrolls, copies and searches the way everything else in that window does. The composer takes `/` for commands and `@` to put a file in front of the agent. The status line carries the live model, the session, and how much context is left.

The first thirty seconds look like this:

1. Type what you want — `@payments.go what breaks if the retry budget is exhausted mid-write?` — and read the answer as it lands. Reads run silently; the shipped envelope allows them.
2. Ask for something that changes the world — a file written, a command run. The call stops in front of you as an **approval card**: the tool, the exact arguments, and the rule that gated it.
3. Answer it with one keystroke. Approve and the call runs and the turn continues; deny and the agent is told so and works around it.

Nothing about that card is beam being careful. The envelope decided it before the surface saw it, so the same call gates the same way in an editor, in a mission, or on your phone. That is the whole idea: see [Human gates and envelopes](/docs/guide/hitl/).

If nobody answers the card, the turn does not sit there burning a connection — it checkpoints, releases the process, and waits as a durable ask you can answer later from anywhere. See [the durable ask](/docs/guide/hitl/#what-a-parked-approval-looks-like).

---

## 6. Scripted and background work

Once the agent does what you want at the keyboard, the same declaration runs without you.

**A program is the caller** — CI, cron, a Makefile. `contenox run` takes the task, prints the report to stdout, and exits 0 when the work landed:

```bash
contenox run reviewer "review payments.go"
contenox run "summarise what changed under ./internal since Friday"
```

With no agent named it runs the preseeded `run` declaration.

**Or fire it and walk away.** A [mission](/docs/guide/missions/) is a one-line intent at a declared agent under a named envelope, with a durable record — reports, plan, and questions that survive the terminal you closed:

```bash
contenox mission fire reviewer "review the payment retry change" --wait
```

---

## 7. Optional editor use

Contenox also runs inside editor or desktop clients that speak ACP. The same agents, model config, tools, and HITL policy are used either way; per the protocol the editor owns the workspace, so a session works in the project you already have open:

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

- [**Your first agent**](/docs/guide/tutorials/first-agent/) — one file, what contenox builds behind it, and where the knobs are
- [CLI reference](/docs/reference/contenox-cli/) — `beam`, `run`, `serve`, and every flag
- [Missions](/docs/guide/missions/) — unattended runs, their envelopes, and the durable record they leave
- [Declaring agents](/docs/guide/agents/) — the full frontmatter, skills, and the tools an agent brings with it
- [Core concepts](/docs/guide/concepts/) — how agents, chains, tasks, and tools fit together
- [Writing a chain by hand](/docs/guide/chains/writing-a-chain/) — for the agent that has outgrown a declaration
- [How contenox compares](/docs/guide/comparison/) — what it shares with the coding agents, and the three things that are built differently
- [MCP integration](/docs/integrations/tools/mcp/) — connect external tools
- [Pairing a machine with a relay](/docs/guide/pairing/) — reach a running session from your phone: one typed key, optional always, free for you and three teammates (one machine each)
- [AI sovereignty & the EU AI Act](/docs/guide/sovereignty/) — hosting, state, and oversight controls you own
