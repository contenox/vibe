# contenox

**Fire work at an agent, under rules you can read.**

An open agentic harness, built around the envelope: a plain file that bounds
what an agent may do unattended — tool allowlists, command policy, budgets,
approval gates. Missions are what envelopes make safe: fire one from a
persistent session, detach, come back to done. Envelopes survive restarts — an
unanswered approval checkpoints the run; answer it later with
`contenox approvals respond`, from any terminal that can reach your models, and
the run resumes exactly once. Terminal CLI, the same
sessions inside Zed, JetBrains, or any ACP editor, and `contenox new` — a
full-screen TUI with chat, plan, shell, and editor-grade file tools in one
persistent coding session. Any model, any MCP server, any OpenAPI spec as
tools, in combination. SQLite. No account.

| The old way | contenox |
| --- | --- |
| A hidden prompt | A file you edit |
| Guessing the blast radius | An envelope you wrote |
| Watching it work | Detached missions |
| One vendor | Any model, any MCP, together |

We don't build toward one model with standing access to everything. You
declare the agent with exactly what it needs — the tools, the model, the
budget, the approval gate — nothing implied, nothing assumed.

Docs: **[contenox.com](https://contenox.com)**

---

## How it compares

Most of what contenox does, the dedicated coding agents also do: a chat loop
with tools, tool calling, MCP servers, provider switching, ACP editor sessions,
sessions in local SQLite, a terminal UI. `contenox new` is a real coding
session — chat, plan, shell, and editor-grade file tools in one persistent
full-screen UI. Aider, OpenCode, Kilo Code and Claude Code all ship that set,
and for pure coding ergonomics — repo mapping, diff application, edit formats —
they are more refined. That part is table stakes, not the argument.

Three things are built differently:

* **The envelope is a separate artifact from the workflow.** The chain says
  what happens; the [envelope](https://contenox.com/docs/guide/hitl/) says what
  is permitted. Two files, authored and versioned independently, both checked
  by `contenox vet`, evaluated before every tool call, fail-closed when nothing
  matches. Hand someone a chain and keep the policy; tighten the policy without
  touching the workflow.
* **Human gates are durable.** An unanswered approval checkpoints the run and
  releases its process. Answer it days later from another terminal with
  [`contenox approvals respond`](https://contenox.com/docs/reference/contenox-cli/#contenox-approvals);
  the verdict is recorded once by a compare-and-swap and the checkpoint is
  claimed before the run rebuilds. The closest analog is workflow
  infrastructure, not a dev tool.
* **Delegation is bounded and attributed.** An envelope may grant an agent
  [a fixed number of answers](https://contenox.com/docs/guide/hitl/#who-may-answer-a-units-question-attention)
  to another agent's questions. The budget is durable, your own answers don't
  spend it, and the record shows who answered.

The cost: the weakest surfaces are the ones you meet first, kernel-enforced
sandboxing is Landlock and Linux-only, and all three differences pay off on day
thirty rather than day one. The long version, with the mechanisms and the rest
of the caveats: **[How contenox compares](https://contenox.com/docs/guide/comparison/)**.

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

<!-- TAG=v0.37.0 -->

```bash
contenox setup                          # pick a provider and model, once
contenox "say hello world in python"    # chat straight from the CLI
contenox chat -e                        # compose a rich prompt in $EDITOR
contenox new                            # full-screen TUI: chat, plan, shell, file edits
contenox resume                         # same TUI, reopening your last session
```

`new` always starts a fresh session; `resume` replays the last active one (or
`--session <name>`). Sessions persist — `contenox session list` and
`contenox session switch <name>` pick past contexts back up. That's it;
sensible defaults do the rest, and `contenox doctor` explains itself when
something is missing.

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

## Editor integration

Contenox speaks the [Agent Client Protocol (ACP)](https://github.com/zed-industries/agent-client-protocol)
over standard I/O — one harness behind every editor session.

### Zed

Add to `~/.config/zed/settings.json`:

```json
{
  "agent_servers": {
    "Contenox": {
      "type": "custom",
      "command": "contenox",
      "args": ["acp"]
    }
  }
}
```

Tool invocations render as interactive cards, approval prompts hook into the
editor's native permission UI, and session history replays when you reopen a
project.

*Step-by-step guides:* [Zed](https://contenox.com/docs/integrations/editors/zed/) | [JetBrains](https://contenox.com/docs/integrations/editors/jetbrains/) | [AionUi](https://contenox.com/docs/integrations/editors/aionui/) | [OpenClaw](https://contenox.com/docs/integrations/editors/openclaw/).

Terminal. Editor. **Contenox**.

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
