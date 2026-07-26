# contenox

**Fire coding work at an agent, under rules you can read.**

An open coding harness. Terminal CLI, the same sessions inside Zed,
JetBrains, or any ACP editor, and beam on the way. Missions run detached
under approval envelopes — plain files, like the system prompts, chains, and
tool allowlists. Any model, any MCP server, any OpenAPI spec as tools, in
combination. SQLite. No account.

| The old way | contenox |
| --- | --- |
| A hidden prompt | A file you edit |
| Guessing the blast radius | An envelope you wrote |
| Watching it work | Detached missions |
| One vendor | Any model, any MCP, together |

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

*Pre-built release downloads and source builds are also available on the [releases page](https://github.com/contenox/beam/releases).*

---

## Quick Start

<!-- TAG=v0.36.0 -->

```bash
contenox setup                          # pick a provider and model, once
contenox "say hello world in python"    # chat straight from the CLI
contenox chat -e                        # compose a rich prompt in $EDITOR
```

Sessions persist — `contenox session list` and `contenox session switch <name>`
pick past contexts back up. That's it; sensible defaults do the rest, and
`contenox doctor` explains itself when something is missing.

---

## What people use it for

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

*Step-by-step guides:* [Zed](https://contenox.com/docs/integrations/editors/zed/) | [JetBrains](https://contenox.com/docs/integrations/editors/jetbrains/) | [AionUi](https://contenox.com/docs/integrations/editors/aionui/).

Terminal. Editor. **beam** — soon.

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
human first (in the terminal or your editor's permission UI), agent shells run
confined with scrubbed environments, and every session leaves reviewable local
state. Approval policies are yours to author — loosen or tighten per chain,
and the harness stays out of your way everywhere else.

---

## Building from source

The CLI is pure Go — no C toolchain, no native dependencies.

```bash
git clone https://github.com/contenox/beam
cd beam
task build        # https://taskfile.dev — or: CGO_ENABLED=0 go build ./cmd/contenox
```

---

Questions? Reach out at **hello@contenox.com**
