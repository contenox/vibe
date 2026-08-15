# contenox

**An agent server.**

Docs: **[contenox.com](https://contenox.com)**

---

## You don't build an agent. You declare one.

An agent is one file:

```markdown
---
name: reviewer
description: Reviews a file for correctness problems
tools: Read, Glob, Grep
---

You are a code reviewer. Read the file you are asked about, then list the
problems you can point at in what you actually read.
```

Drop it in `.contenox/agents/` and the next run picks it up. No build step, no
plugin API, no release:

```
.contenox/
  agents/
    reviewer.md      one agent
    triage.md        another
  agents.toml        the knobs a declaration cannot reach
```

**Already have agents?** `.claude/agents/` and `.agents/agents/` are read where
they are. Nothing to move or convert — it is the same file.

```bash
contenox agent list
contenox mission fire reviewer "review the payment retry change" --wait
```

An agent can also bring its own tools — an MCP server, or any OpenAPI service:

```yaml
mcpServers:
  filesystem:
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/data"]
remoteTools:
  billing:
    url: https://internal.example.com
    spec: https://internal.example.com/openapi.json
```

Registered scoped to that agent, reachable by no other one, retired when you
delete the file. What you connected yourself with `contenox mcp add` stays
yours and is never touched.

Behind each declaration contenox builds a **chain** that says what happens and a
**policy** that says what is permitted. Every policy denies `.ssh`, `.aws` and
`.kube` under every permission setting.

You do not maintain those files — edit the declaration and they follow.

Model routing is configuration too: `contenox backend add`, `contenox config set
default-provider`.

The same agent runs in the terminal, headless in CI, inside an ACP editor, and
as a unit the fleet dispatches.

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

Pre-built binaries are on the [releases page](https://github.com/contenox/contenox/releases).

---

## Quick Start

<!-- TAG=v0.40.5 -->

```bash
contenox setup                          # pick a provider and model, once
contenox "say hello world in python"    # chat straight from the CLI
contenox chat -e                        # compose a rich prompt in $EDITOR
contenox acp                            # speak ACP over stdio to any ACP client
```

Sessions persist: `contenox session list` and `contenox session switch <name>`
pick past contexts back up. `contenox doctor` reports anything missing.

All of it runs locally, with no account. To reach a running session from your
phone — reading the transcript, answering approvals — pair the machine with the
hosted relay: sign in at [app.contenox.com](https://app.contenox.com), tap
**Pair device**, and enter the key as `/pair <key>`. Free for you and three
teammates, one machine each, opt-in per machine.
[How pairing works.](https://contenox.com/docs/guide/pairing/)

---

## What people use it for

* **Standing, scheduled agents** — declare one, call it from cron or CI. Each
  run starts clean.
* **Reviewing diffs** — run tests, summarize risks, keep destructive operations
  behind an approval.
* **Release evidence** — aggregate git logs, PRs, tickets and CI output into
  changelogs and reviewer packets.
* **Wrapping internal APIs** — expose a subset of an OpenAPI spec as a tool,
  with sensitive arguments filled in by config rather than the model.
* **Repo chores** — ingest an issue, generate a patch, run checks, draft the PR
  description.
* **Live operations** — query dashboards, scripts or MCP tools under scoped
  policies instead of broad credentials.

---

## Connect your stack

Anything reachable over MCP, an OpenAPI spec, or a shell command becomes a tool
your agents can name:

```bash
# Connect any Model Context Protocol (MCP) server
contenox mcp add notion https://mcp.notion.com/mcp --auth-type oauth

# Wrap an internal HTTP API using its OpenAPI specification
contenox tools add erp_billing \
  --url https://erp.internal.example.com \
  --spec ./billing-subset.yaml

# Bind the local shell under your policy
contenox --shell "check Proxmox and flag anything red"
```

---

## Backends

Mix local and hosted freely:

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

## Guardrails

Gated actions ask a human first, in the terminal or your editor's permission UI,
and every session leaves reviewable local state on disk. Loosen or tighten the
rules in `agents.toml`.

The gate sits at the tool layer: every call is checked against the policy before
it runs. Shells started by an agent are ordinary child processes and inherit the
runtime's environment. The
[sandbox](https://contenox.com/docs/guide/agent-sandbox/) — Landlock filesystem
and exec confinement, scrubbed environment, Linux-only — confines foreign agent
code instead.

---

## Building from source

Pure Go, no C toolchain:

```bash
git clone https://github.com/contenox/contenox
cd contenox
task build        # https://taskfile.dev — or: CGO_ENABLED=0 go build ./cmd/contenox
```

---

Questions: **hello@contenox.com**
