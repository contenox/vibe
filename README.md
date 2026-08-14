# contenox

**An agent server.**

Docs: **[contenox.com](https://contenox.com)**

---

## You don't build an agent. You declare one.

An agent is files in `.contenox/` — discovered on start, live on the next run,
reviewable in a pull request:

```
.contenox/
  chain-agent-acp.json          # the editor agent: tasks, routing, tools, branching
  chain-agent-run.json          # the headless agent, for cron and CI
  chain-planner-default.json    # sub-chains the agents call
  hitl-policy-default.json      # what needs a human, and when
  hitl-policy-strict.json       # the same agent, tighter — swap per environment
AGENTS.md                       # project context, loaded into every session
```

A chain is a state machine over tasks. Edit it and the next invocation runs the
new one — no build step, no plugin API, no release:

```json
{
  "$schema": "https://contenox.com/schema/task-chain.schema.json",
  "id": "chain-triage",
  "description": "Classify an incoming issue, then file it or drop it.",
  "token_limit": 131072,
  "tasks": [
    {
      "id": "classify",
      "handler": "route",
      "system_instruction": "Classify the issue: bug, feature, or noise.",
      "execute_config": { "model": "{{var:model}}", "provider": "{{var:provider}}" },
      "transition": {
        "on_failure": "",
        "branches": [
          { "operator": "equals",  "when": "bug", "goto": "file_ticket" },
          { "operator": "default", "when": "",    "goto": "end" }
        ]
      }
    },
    {
      "id": "file_ticket",
      "handler": "chat_completion",
      "system_instruction": "Open a ticket with a clear title and repro steps.",
      "execute_config": { "model": "{{var:model}}", "provider": "{{var:provider}}" },
      "transition": {
        "on_failure": "",
        "branches": [ { "operator": "default", "when": "", "goto": "end" } ]
      }
    }
  ]
}
```

Both file kinds are JSON Schema–validated — [task
chains](https://contenox.com/schema/task-chain.schema.json) and [HITL
policies](https://contenox.com/schema/hitl-policy-v1.schema.json), generated
from the Go types that load them. Keep the `$schema` line and your editor
completes and checks them as you type; CI checks them too.

Model routing is the same story — `contenox backend add`, `contenox config set
default-provider`. Nothing about which model, which tool, or which action needs
a human is compiled in.

**The same chain runs unchanged** in the terminal, headless in CI, inside an ACP
editor, and as a unit the fleet dispatches. Tightening what an agent may do is a
diff.

> Files named `chain-agent-*.json` are also discovered as fleet-dispatchable
> agents. The shipped ones are always available; surfacing your *own* in the
> agent roster currently needs `contenox config set opt-in-beta true`.

---

## Install

```bash
curl -fsSL https://contenox.com/install.sh | sh
```

Prefer to read it first?

```bash
curl -fsSLO https://contenox.com/install.sh
less install.sh
sh install.sh
```

*Pre-built release downloads and source builds are also available on the [releases page](https://github.com/contenox/contenox/releases).*

---

## Quick Start

<!-- TAG=v0.40.2 -->

```bash
contenox setup                          # pick a provider and model, once
contenox "say hello world in python"    # chat straight from the CLI
contenox chat -e                        # compose a rich prompt in $EDITOR
contenox acp                            # speak ACP over stdio to any ACP client
```

Sessions persist — `contenox session list` and
`contenox session switch <name>` pick past contexts back up. That's it;
sensible defaults do the rest, and `contenox doctor` explains itself when
something is missing.

Everything above is local: on your machine, no account. When you do want
a running session reachable from elsewhere — reading the transcript and
answering approvals from your phone — pair the machine with the hosted relay:
sign in at [app.contenox.com](https://app.contenox.com), tap **Pair
device**, and type the key into the session as `/pair <key>`. Free for you and
three teammates (one machine each), opt-in per machine, and an install that
never pairs contacts no relay at all.
[How pairing works.](https://contenox.com/docs/guide/pairing/)

---

## What people use it for

* **Running standing, scheduled agents:** declare a narrow agent once —
  triage this inbox, watch this feed — and call it from cron or CI every
  morning; it starts clean each run and never carries yesterday's job into
  today's.
* **Reviewing diffs:** run tests, summarize risks, and keep destructive
  operations behind an approval prompt.
* **Drafting release evidence:** aggregate git logs, PRs, tickets, and CI
  output into changelogs and reviewer packets.
* **Wrapping internal APIs:** expose a curated subset of an OpenAPI spec as a
  tool, with the sensitive arguments filled in by config, not by the model.
* **Automating repo chores:** ingest an issue, generate a patch, run local
  checks, draft the PR description.
* **Inspecting live operations:** query dashboards, shell scripts, or MCP
  tools through tightly scoped policies instead of broad credentials.

The unit of repeatability is the **Chain**: a declarative, version-controlled
file that defines prompts, model routing, tools, retries, branching, and where
a human gets the final word. The same chain runs identically in the terminal,
in headless scripts, and inside any ACP editor.

---

## Connect your stack

Anything reachable via an MCP server, an OpenAPI spec, or a shell command can
become a tool in a chain:

```bash
# Connect any Model Context Protocol (MCP) server
contenox mcp add notion https://mcp.notion.com/mcp --auth-type oauth

# Wrap an internal HTTP API using its OpenAPI specification
contenox tools add erp_billing \
  --url https://erp.internal.example.com \
  --spec ./billing-subset.yaml

# Bind the local shell under a chain policy
contenox --shell "check Proxmox and flag anything red"
```

---

## Backends

Model routing is configuration, not code. Mix local and hosted freely:

```bash
# Local & private-network inference
contenox backend add ollama --type ollama
contenox backend add myvllm --type vllm --url http://gpu-host:8000

# Hosted providers
contenox backend add openai    --type openai    --api-key-env OPENAI_API_KEY
contenox backend add anthropic --type anthropic --api-key-env ANTHROPIC_API_KEY
contenox backend add gemini    --type gemini    --api-key-env GEMINI_API_KEY

# Defaults
contenox config set default-provider ollama
contenox config set default-model    qwen2.5:7b
```

Also supported: Gemini, Vertex AI, and Amazon Bedrock.

---

## Guardrails, without the nagging

Defaults are safe so you don't have to think about them: gated actions ask a
human first (in the terminal or your editor's permission UI), and every session
leaves reviewable local state. Approval policies are yours to author — loosen or
tighten per chain, and the harness stays out of your way everywhere else.

Know exactly what that gate is. Everything you run today — `chat`, `run`,
`new`, an editor `acp` session, and the mission units the fleet dispatches — is
contenox's own chains in contenox's own process, bounded by the approval gate and
the chain's tool policy. That is a gate at the tool layer, not a kernel sandbox:
their shells are ordinary child processes and inherit the runtime's environment.
The [sandbox](https://contenox.com/docs/guide/agent-sandbox/) — Landlock-enforced
filesystem and exec confinement, scrubbed environment, Linux-only and fail-closed
— is what confines a *foreign* agent, code contenox did not write; registering
one is not exposed yet, so nothing on a stock install takes that path.

---

## Building from source

The CLI is pure Go — no C toolchain, no native dependencies.

```bash
git clone https://github.com/contenox/contenox
cd contenox
task build        # https://taskfile.dev — or: CGO_ENABLED=0 go build ./cmd/contenox
```

---

Questions? Reach out at **hello@contenox.com**
