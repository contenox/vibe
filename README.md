# contenox

**An open agentic harness. Automation you control and own.**

Docs: **[contenox.com](https://contenox.com)**

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

<!-- TAG=v0.40.1 -->

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

Everything above is local: SQLite on your machine, no account. When you do want
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
