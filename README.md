# contenox

**Declare an agent in one Markdown file. Run it in your terminal.**

Every action it takes is checked against policy you wrote — and when it needs
you, it waits, durably: answer from your terminal or your phone, days later, and
the run resumes exactly once.

Docs: **[contenox.com](https://contenox.com)**

---

## Start here

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

<!-- TAG=v0.41.0 -->

```bash
contenox setup                          # pick a provider and model, once
contenox beam                           # the front door: talk to an agent here
```

`contenox beam` is the first-party terminal client. The transcript is your
native scrollback, the composer takes `/` commands and `@` file mentions, and a
gated tool call raises an approval card inline — one keystroke answers it. Bare
`contenox` on a terminal opens beam.

`contenox doctor` reports anything missing; its first line is the verdict:

```
Ready: yes — run: contenox beam
```

`contenox init` scaffolds a project's `.contenox/` — agents, envelopes, config —
and `contenox vet` checks a policy before anything runs under it. Sessions
persist: `contenox session list` and `contenox session switch <name>` pick past
contexts back up.

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

## Three shapes

The same runtime, the same declarations, the same envelope. What differs is who
is accountable for the machine it runs on.

**`contenox beam`** — you, at the keyboard, on your own device. The terminal
client above: filesystem and terminal work natively, and a gated call stops in
front of you.

```bash
contenox beam                           # or just: contenox
```

**`contenox run`** — a program is the caller: CI, cron, another agent. It runs
the task and prints the report to stdout, exit 0 when the work landed and
nonzero when it did not, using the tools on that machine.

```bash
contenox run "summarise what changed under ./internal since Friday"
contenox run reviewer "review the payment retry change"
```

With no agent named it runs the preseeded `run` declaration.

**`contenox serve`** — the organization's shape: a standing host on a box
somebody else looks after. It serves exactly one workspace, fixed when you
launch it, and it has **no filesystem and no terminal tools, ever** — every
capability it has is an MCP server you attached. Connectors and event triggers
drive it through the relay, and it can run on Postgres, NATS and Valkey when one
host is not enough.

```bash
contenox serve                          # the workspace is your home directory
contenox serve ~/src/api                # or the path you name
```

One instance serves one workspace. There is no workspace picker anywhere,
because there is nothing to pick: the app discovers instances and the sessions
they are already holding.

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

The gate sits at the tool boundary: every call is checked against the envelope
before it leaves contenox, whether it is headed for your terminal or an MCP
server you attached. Gated actions ask a human first — the approval card in
beam, your editor's permission UI, or your phone.

**From your phone.** Everything above runs locally, with no account. To reach a
running session from elsewhere — reading the transcript, answering approvals —
pair the machine with the hosted relay: sign in at
[app.contenox.com](https://app.contenox.com), tap **Pair device**, and enter the
key as `/pair <key>` in the session. The machine dials out and sends exactly two
things, the key and its hostname. Free for you and three teammates, one machine
each, opt-in per machine.
[How pairing works.](https://contenox.com/docs/guide/pairing/)

---

## Integrations

**Editors.** Zed, JetBrains, AionUi and OpenClaw spawn contenox as an ACP
subprocess over stdio — no plugin lock-in, and approvals route through the
editor's own permission UI. Per the protocol the editor owns the workspace, so
the session works in the project you already have open.

```bash
contenox acp                            # speak ACP over stdio to any ACP client
```

**Missions and events.** `contenox mission fire` sends a one-line intent to a
declared agent under a named envelope and leaves a durable record; internal
domain events land in a durable log where operator-authored `trigger-*.json`
files fire chains from them (opt-in, beta).

---

## We ship no tools. That is the point.

contenox owns the tool boundary and you decide what stands on the other side of
it. Every tool you do not need is tokens burned on every turn, and one more
thing to govern. Tools cross that boundary two ways, and both are yours to
choose.

**From the client.** beam and every ACP client — your editor — carry `fs/*` and
`terminal/*` as capabilities. contenox forwards the call and the client performs
it, in the workspace already open. `local_fs` is five tools: `read_file`,
`write_file`, `edit_file`, `sed`, `read_file_range`. Listing and search go
through the shell, on the client's side of the line. `contenox serve` has
neither, by construction.

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

## Guardrails

Every run is bounded by an **envelope**: a JSON policy naming what passes
silently, what stops for a human, and what is denied outright, plus hard
ceilings on tool calls and tokens. Anything no rule matches fails closed — it
asks. Six presets ship with `contenox init`, and the knobs a declaration cannot
reach live in `agents.toml`. Every session leaves reviewable local state on
disk.

The [sandbox](https://contenox.com/docs/guide/confinement/sandbox/) — Landlock
filesystem and exec confinement, scrubbed environment, Linux-only — confines
foreign agent code you choose to run locally.

---

## What people use it for

* **Standing, scheduled agents** — declare one, call it with `contenox run` from
  cron or CI. Each run starts clean.
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

## The argument

Before Apache, serving a website meant writing your own server: parse the
request, hold the connection, decide what to send — all of it welded to the
content it existed to deliver. Apache made serving something you **install**,
and HTML the thing you **author**. Everyone building an agent today is back on
the wrong side of that line, hand-rolling the same machine — the loop, the tool
gate, the approval flow, session persistence — welded to one prompt and
rewritten at the next company.

contenox makes that machine infrastructure and the declaration the artifact.
Where the analogy stops: Apache shipped no browser, and contenox ships one.
`contenox beam` is it — a first-party client, in-tree, so the front door is
never somebody else's product. It is still a client, and the policy it renders
is enforced underneath it rather than by it.

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
