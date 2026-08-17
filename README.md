# contenox

**An agent server.**

Docs: **[contenox.com](https://contenox.com)**

---

## The argument

Before Apache, serving a website meant writing your own server: parse the
request, hold the connection, decide what to send — all of it welded to the
content it existed to deliver. Apache made serving something you **install**,
and HTML became the thing you **author**.

Everyone building an agent today is back on the wrong side of that line,
hand-rolling the same machine: the loop, the tool gate, the approval flow,
session persistence — welded to one prompt, rewritten at the next company.

contenox makes that machine infrastructure and the declaration the artifact.
Install the server. Author the agent.

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
they are. Claude Code, Copilot, Cursor, OpenCode and Antigravity declarations
all import — nothing to move or convert, it is the same file.

```bash
contenox agent list
contenox mission fire reviewer "review the payment retry change" --wait
```

**A directory of declarations is a workflow.** The `agent.md` at the top reads
the request and answers with one label; the branch of that name takes it from
there, with its own instruction, its own tools, its own budget:

```
.contenox/agents/
  triage/
    agent.md         reads the request, answers with one label
    code/
      agent.md       the branch that label routes to
      recovery.md    its second attempt, when the first stops short
    docs/
      agent.md       tools: Read, Glob, Grep — it cannot write
    failure.md       what it says when every branch has given up
```

`default:` in the router's frontmatter names the branch an unrecognised answer
falls to — the narrowest one, never the most capable.

Behind each declaration contenox compiles a **chain** that says what happens and
a **policy** that says what is permitted, into `.generated/`. Both are JSON
Schema-validated, both are yours to read, and neither is yours to maintain —
edit the declaration and they follow. Every policy denies `.ssh`, `.aws` and
`.kube` under every permission setting.

Model routing is configuration too: `contenox backend add`, `contenox config set
default-provider`.

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

## First run

<!-- TAG=v0.41.0 -->

```bash
contenox init                           # scaffold .contenox/ — agents, envelopes, config
contenox setup                          # pick a provider and model, once
contenox agent list                     # what is declared, and where it was read from
contenox mission fire triage "sort the tickets that came in overnight"
```

`contenox doctor` reports anything missing, and `contenox vet` checks a policy
before anything runs under it. Sessions persist: `contenox session list` and
`contenox session switch <name>` pick past contexts back up.

---

## Two shapes

**Your editor launches it.** Zed, JetBrains, AionUi and OpenClaw start contenox
as an ACP subprocess over stdio — no plugin lock-in, and approvals route through
the editor's own permission UI.

```bash
contenox acp                            # speak ACP over stdio to any ACP client
```

Everything runs locally, with no account. To reach a running session from your
phone — reading the transcript, answering approvals — pair the machine with the
hosted relay: sign in at [app.contenox.com](https://app.contenox.com), tap
**Pair device**, and enter the key as `/pair <key>` in the session. Free for you
and three teammates, one machine each, opt-in per machine.
[How pairing works.](https://contenox.com/docs/guide/pairing/)

**Or it runs as a host.** Same runtime, no editor in front of it: it holds the
relay connection open and stays up until you stop it, taking missions and
reaching the MCP servers you attached.

```bash
contenox serve                          # a host on a headless box
```

---

## The durable ask

Any harness can pause for a human while it holds the connection open. Holding a
connection is not the hard part — surviving the wait is.

A run that stops for a person checkpoints where it stopped, saves the ask, and
releases the process. Restart the box, close the laptop, let days pass: when the
answer arrives, the run resumes from that exact point, exactly once.

```bash
contenox approvals list
contenox approvals respond 8f3c --answer "yes, send them"
contenox inbox list
```

---

## We ship no tools. That is the point.

Apache shipped modules, not websites. contenox owns the tool boundary and you
decide what stands on the other side of it. Every tool you do not need is tokens
burned on every turn, and one more thing to govern. Tools cross that boundary two
ways, and both are yours to choose.

**From the client.** An ACP client — your editor — negotiates
`fs/*` and `terminal/*` as capabilities. contenox forwards the call and the
client performs it, in the workspace you already have open. `local_fs` is five
tools: `read_file`, `write_file`, `edit_file`, `sed`, `read_file_range`. Listing
and search go through the shell, on the client's side of the line.

**From the operator.** Anything reachable over MCP or described by an OpenAPI
spec becomes a policy-scoped tool your agents can name:

```bash
# Connect any Model Context Protocol (MCP) server
contenox mcp add notion https://mcp.notion.com/mcp --auth-type oauth

# Wrap an internal HTTP API using its OpenAPI specification
contenox tools add erp_billing \
  --url https://erp.internal.example.com \
  --spec ./billing-subset.yaml
```

A declaration can also bring its own, scoped to that agent, reachable by no
other one, retired when you delete the file:

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

What you connected yourself with `contenox mcp add` stays yours and is never
touched.

---

## What people use it for

* **Standing, scheduled agents** — declare one, call it from cron or CI. Each
  run starts clean.
* **Request processing** — intake, classify, draft, hold for a human, send.
  The hold is the feature.
* **Wrapping internal APIs** — expose a subset of an OpenAPI spec as a tool,
  with sensitive arguments filled in by config rather than the model.
* **Release evidence** — aggregate git logs, PRs, tickets and CI output into
  changelogs and reviewer packets.
* **Live operations** — query dashboards, scripts or MCP tools under scoped
  policies instead of broad credentials.

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

Every run is bounded by an **envelope**: a JSON policy naming what passes
silently, what stops for a human, and what is denied outright, plus hard
ceilings on tool calls and tokens. Anything no rule matches fails closed — it
asks. Six presets ship with `contenox init`, and the knobs a declaration cannot
reach live in `agents.toml`.

The gate sits at the tool boundary: every call is checked against the policy
before it leaves contenox, whether it is headed for the client's terminal or an
MCP server you attached. Gated actions ask a human first — in the terminal, in
your editor's permission UI, or on your phone — and every session leaves
reviewable local state on disk.

The [sandbox](https://contenox.com/docs/guide/confinement/sandbox/) — Landlock
filesystem and exec confinement, scrubbed environment, Linux-only — confines
foreign agent code you choose to run locally.

---

## Managed

We provision and run contenox agents for you, on your terms. Tell us what the
work is and we will get you set up: **hello@contenox.com** — or see the hosted
app at [app.contenox.com](https://app.contenox.com).

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
