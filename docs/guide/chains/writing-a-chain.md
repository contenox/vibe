---
title: "Writing a chain by hand"
description: Walk from a blank file to a working authored chain in five edits — for the agent that has outgrown a declaration.
order: 1
---

# Writing a chain by hand

Most agents never need this page. An agent is [a Markdown declaration](/docs/guide/agents/) plus [`agents.toml`](/docs/reference/agents-config/), and contenox builds the chain behind it.

You are here because you need something a declaration cannot say: a branch, a different model per step, a recovery path, a point where a human is required. Then you write the state machine yourself, and the engine runs exactly what you wrote. This page walks you from a blank file to a working chain in five edits.

If you haven't installed Contenox yet, do the [Quickstart](/docs/guide/quickstart/) first. If you have not written an agent yet, do [your first agent](/docs/guide/tutorials/first-agent/) — it is the shorter road and probably the right one.

---

## Workspaces

`contenox init` creates or refreshes two kinds of state:

**A project-local workspace marker** — `.contenox/workspace.id` in the current directory. This is like `.git/` — it marks this directory tree as a Contenox workspace. The engine walks up from your current directory looking for this marker to resolve which workspace you're in.

**Global runtime files** — `~/.contenox/` stores everything that's shared across workspaces: your agents, the envelopes they run under, the SQLite database, and the shipped chains under `system/`.

```
~/.contenox/                    ← global (shared across all workspaces)
├── local.db                    ← SQLite: backends, config, sessions, MCP registrations
├── agents.toml                 ← the knobs a declaration cannot reach, and the envelopes
├── agents/                     ← your agents, one Markdown file each
├── .generated/                 ← derived, rewritten every run: never edit, never commit
│   ├── hitl-policy-default.json    ← transpiled from [envelopes.default]
│   ├── hitl-policy-strict.json     ← …from [envelopes.strict]
│   ├── hitl-policy-acpx.json       ← …and so on, one per declared envelope
│   └── chain-agent-*.json          ← the chains your declarations compiled to
└── system/                     ← the shipped chains: machinery, not yours to author
    ├── chain-planner-default.json  ← the default mission planner
    ├── chain-compact-default.json  ← history compaction
    └── chain-fim-default.json      ← editor autocomplete

./my-project/.contenox/         ← project-local workspace marker
└── workspace.id                ← unique workspace ID
```

Nothing seeds a `hitl-policy-*.json` at the top level any more. Write one there yourself and it shadows the transpiled envelope of the same name — which is the supported way to take a machine-specific policy out of a shared `agents.toml`.

To make any directory a workspace, run `contenox init` inside it. Workspace-scoped config (like `default-chain` and `hitl-policy-name`) is stored per-workspace in the SQLite database.

**Taking ownership of a shipped chain is a copy.** Files resolve by name — the workspace `.contenox/` first, then `~/.contenox/`, then `~/.contenox/system/`. So copying one up a level makes it yours, and `contenox init` will not write over it or put a shipped copy back underneath.

> **Note:** `contenox init --local` seeds the shipped chains into the workspace `.contenox/` for you — the supported way to create workspace-local overrides without copying files by hand. Envelopes need no seeding: put an `[envelopes.<name>]` section in the workspace `agents.toml` and it transpiles into `.contenox/.generated/`, ahead of the global one. `contenox doctor` lists which workspace copies are currently shadowing global ones.

## What `contenox init` already gave you

Look in `~/.contenox/system/`. Every chain file follows the `chain-<role>-<variant>.json` naming convention ([the full grammar](/docs/guide/chains/naming/)).

You don't have to start there. You can write your own.

Chains live as files in `~/.contenox/` (and your workspace `.contenox/`). Name
one `chain-agent-<something>.json` and it's discovered as a fleet-dispatchable
agent, fireable by its `id` (below); a session instead falls back to the
configured `default-chain`, and ACP editor sessions run the workspace's
default chain the same way.

---

## A minimal chain

Create `.contenox/chain-agent-my-chain.json`:

```json
{
  "id": "my-chain",
  "tasks": [
    {
      "id": "answer",
      "handler": "chat_completion",
      "execute_config": {
        "model": "qwen3:8b",
        "provider": "ollama"
      },
      "transition": {
        "branches": [
          { "operator": "default", "goto": "end" }
        ]
      }
    }
  ]
}
```

Fire it:

```bash
contenox mission fire my-chain "what is the capital of France?" --wait
```

That's the smallest working chain: one task, one default branch out. Now we'll author behavior into it.

---

## Edit 1 — Set a system prompt

Add `system_instruction` to the task. This is the agent's persona for this chain — it lives in your file, not in vendor code.

```json
{
  "id": "answer",
  "handler": "chat_completion",
  "system_instruction": "You are a terse senior engineer. One sentence answers. No preamble.",
  "execute_config": {
    "model": "qwen3:8b",
    "provider": "ollama"
  },
  "transition": { "branches": [{ "operator": "default", "goto": "end" }] }
}
```

The agent now answers in your voice, not the model's default voice.

---

## Edit 2 — Pick the model (and a fallback)

`execute_config.model` and `execute_config.provider` choose the backend. Use `models[]` and `providers[]` to author a fallback policy — the engine tries them in order. The `execute_config` block on the task becomes:

```json
{
  "execute_config": {
    "models": ["qwen3:8b", "gpt-5-mini"],
    "providers": ["ollama", "openai"],
    "temperature": 0.2
  }
}
```

When the local model is unreachable, the chain falls back to OpenAI in the order you listed.

See [the providers guide](/docs/integrations/providers/ollama/) for backend setup.

---

## Edit 3 — Branch on the output

A single task is a function call. A chain becomes interesting when it branches. Add a second task and route to it conditionally.

```json
{
  "id": "my-chain",
  "tasks": [
    {
      "id": "classify",
      "handler": "route",
      "system_instruction": "Classify the message urgency. Respond 'urgent' or 'normal'.",
      "execute_config": { "model": "qwen3:8b", "provider": "ollama" },
      "transition": {
        "branches": [
          { "operator": "equals", "when": "urgent", "goto": "escalate" },
          { "operator": "equals", "when": "normal", "goto": "respond" },
          { "operator": "default", "goto": "respond" }
        ]
      }
    },
    {
      "id": "escalate",
      "handler": "chat_completion",
      "system_instruction": "This is urgent. Draft a one-line page to on-call.",
      "execute_config": { "model": "qwen3:8b", "provider": "ollama" },
      "transition": { "branches": [{ "operator": "default", "goto": "end" }] }
    },
    {
      "id": "respond",
      "handler": "chat_completion",
      "system_instruction": "Reply briefly and helpfully.",
      "execute_config": { "model": "qwen3:8b", "provider": "ollama" },
      "transition": { "branches": [{ "operator": "default", "goto": "end" }] }
    }
  ]
}
```

You authored the labels (`urgent` / `normal`) and the routing — the route set is just the branches you can read. See [Transitions & branching](/docs/specification/transitions/) for all available operators (`equals`, `contains`, `starts_with`, `ends_with`, `edge_traversed_at_least`, `default`).

---

## Edit 4 — Constrain the tool policy

If the task uses tools, you author the policy. Allowlists, denylists, per-tool config — every constraint is a key.

```json
{
  "execute_config": {
    "model": "qwen3:8b",
    "provider": "ollama",
    "tools": ["local_shell", "local_fs"],
    "tools_policies": {
      "local_shell": {
        "_allowed_commands": "ls,cat,grep,git",
        "_denied_commands": "sudo,rm,dd"
      },
      "local_fs": {
        "_max_read_bytes": "1048576"
      }
    }
  }
}
```

`_allowed_commands` and `_denied_commands` constrain what `local_shell` can run for this task, independent of any other chain.

---

## Edit 5 — Add a retry policy

Transient failures shouldn't kill a CI step. Author the retry behavior in the chain:

```json
{
  "execute_config": {
    "model": "qwen3:8b",
    "provider": "ollama",
    "retry_policy": {
      "max_attempts": 4,
      "initial_backoff": "1s",
      "max_backoff": "30s",
      "jitter": 0.25,
      "rate_limit_min_wait": "10s"
    }
  }
}
```

Combine `retry_policy` with `transition.on_failure` to decide what happens when something goes wrong: retry, route to a recovery task, or escalate.

---

## What you've written

- A system prompt
- Model selection with a fallback policy
- Branching with a routing operator
- A tool policy with allowlists
- A retry policy with backoff and jitter

Switch providers by changing `execute_config.provider` and `execute_config.model` — the rest of the chain is unchanged.

## Next

- [Annotated examples](/docs/specification/examples/) — five longer chains, fully commented
- [Handlers reference](/docs/specification/handlers/) — every available task type
- [Transitions & branching](/docs/specification/transitions/) — operators, edges, and `on_failure`
- [Use cases](/docs/use-cases/) — end-to-end recipes for real workflows
