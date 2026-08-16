---
title: "An agent that files notes into Obsidian"
description: Build a working agent from nothing — a folder of raw notes goes in, a tidy Obsidian vault comes out, and the agent physically cannot write anywhere else. Every file explained line by line.
order: 3
---

# Tutorial: an agent that files notes into Obsidian

You have a folder of raw text — meeting notes, reading notes, half-formed ideas.
You want each one turned into a proper Obsidian note: frontmatter, tags, a
summary, the facts kept intact. And you want the agent to be physically unable
to touch anything except the vault.

That is what you will build here. Two files, one command.

Most agents are one Markdown file and nothing else — that road is
[your first agent](/docs/guide/tutorials/first-agent/), and it is shorter. This
one takes the other road on purpose: the write fence has to be a guarantee
rather than an instruction, and the loop has a hard round budget. So you write
the chain and the envelope yourself, and the engine runs exactly what you wrote.

The integration with Obsidian is the part with no work in it: **an Obsidian
vault is a folder of markdown files.** There is no API to call and no plugin to
install. Point the agent's filesystem tool at the vault folder and it is
integrated. (If you want the agent to talk to a *running* Obsidian — the open
note, in-app search — see [When to use the Obsidian MCP server
instead](#when-to-use-the-obsidian-mcp-server-instead) at the end.)

## Before you start

You need contenox and a model. If you have not done this yet:

```bash
curl -fsSL https://contenox.com/install.sh | sh
contenox setup
```

`contenox setup` asks which provider and model to use and stores the answer. Any
provider works — Ollama on your own machine, or a hosted one. Everything below
follows whatever you chose there, so nothing in this tutorial is tied to a
particular model.

Check it is ready:

```bash
contenox doctor
```

The first line should read `Ready: yes`.

## 1. Make the folders

```bash
mkdir -p ~/note-filer/inbox
cd ~/note-filer
```

`inbox/` is where raw notes land. For the vault, either use a real one:

```bash
ln -s ~/Documents/MyVault vault
```

…or make a throwaway one to try this safely first:

```bash
mkdir vault
```

Drop a few text files into `inbox/`. Anything will do — the messier the better,
since that is what you actually have. Here is one to test with:

```bash
cat > inbox/meeting-notes.txt <<'EOF'
Call with Dana, 3 March.
Postgres connection pool is exhausting under load - pgbouncer sits in session mode,
should be transaction mode. Dana owns the change, wants it behind a flag.
Also: the nightly backup job overlaps the ETL window. Move backup to 02:00 UTC.
EOF
```

## 2. Mark the folder as a workspace

```bash
contenox init
```

This creates `.contenox/` with a workspace marker. It matters for one reason:
files you put in `.contenox/` are found by name from this directory, and
settings you make here apply to this project rather than globally. Both of the
files you are about to write go in there.

## 3. Write the chain

The chain is *what the agent does*. Discovery registers every `chain-agent-*.json`
file as a dispatchable agent, using its `id` — not the filename — as the agent's
name, so save this as `.contenox/chain-agent-vaultfiler.json`:

```json
{
  "$schema": "https://contenox.com/schema/task-chain.schema.json",
  "id": "vaultfiler",
  "description": "Reads every file in inbox/ and files each one into an Obsidian vault as a note.",
  "token_limit": 131072,
  "tasks": [
    {
      "id": "file_notes",
      "handler": "chat_completion",
      "system_instruction": "You file raw notes into an Obsidian vault.\n\nThe folder `inbox/` holds raw text files. The folder `vault/` is an Obsidian vault.\n\nDo this, using your tools:\n1. Run `ls inbox` over the shell to see every file.\n2. For EACH file: read it, then write ONE note into `vault/`.\n\nEach note you write must be a markdown file named after its topic in Title Case (e.g. `vault/Connection Pool Exhaustion.md`) and must contain, in this order:\n- YAML frontmatter delimited by --- lines, with `source:` set to the inbox filename it came from, and `tags:` as a YAML list of 2-4 lowercase topic tags.\n- An `## Summary` section: two or three sentences in your own words.\n- An `## Details` section: the concrete facts, as bullets. Keep names, numbers and settings exactly as written.\n- An `## Open questions` section, only when the source leaves something genuinely unresolved.\n\nWrite one note per inbox file. Do not modify anything in `inbox/`. When every file has a note, reply with a one-line summary of what you filed and stop.",
      "execute_config": {
        "model": "{{var:model}}",
        "provider": "{{var:provider}}",
        "tools": ["local_fs", "local_shell"],
        "tools_policies": {
          "local_fs": { "_allowed_dir": ".", "_max_read_bytes": "262144" }
        },
        "max_tokens": 8192
      },
      "transition": {
        "branches": [
          { "operator": "edge_traversed_at_least", "edge": "file_notes->run_tools", "when": "24", "goto": "end" },
          { "operator": "equals", "when": "tool_call", "goto": "run_tools" },
          { "operator": "default", "when": "", "goto": "end" }
        ]
      }
    },
    {
      "id": "run_tools",
      "handler": "execute_tool_calls",
      "transition": {
        "branches": [ { "operator": "default", "when": "", "goto": "file_notes" } ]
      }
    }
  ]
}
```

Line by line:

**`"$schema"`** points at the [published JSON Schema](/schema/task-chain.schema.json).
Editors read it and give you completion and validation as you type. It is worth
keeping — see [Let your editor check the files](#let-your-editor-check-the-files).

**`"token_limit": 131072`** is the context budget, and it is **not optional in
practice**. The engine sizes the maximum tool result from what is left of this
budget, so leaving it out works out to zero and every tool call comes back as
`tool_result_too_large` — even reading a 300-byte file. Use the context window of
the model you configured; see [chain structure](/docs/specification/#chain-structure).

**Two tasks, and the loop between them.** `file_notes` asks the model. If the
model wants to call a tool, the transition sends it to `run_tools`, which
executes the call and goes straight back to `file_notes` with the result. That
back-edge is the agent loop: think, act, see what happened, think again. It is
not a framework feature — it is two lines of JSON pointing at each other.

**`edge_traversed_at_least`** is the way out. After 24 trips around that loop the
chain ends regardless. Without it, a confused model loops until something else
stops it.

**`"tools": ["local_fs", "local_shell"]`** grants exactly these two toolsets. The
default is *nothing* — a task has no tools until this list says otherwise, so an
agent cannot reach for a shell you never mentioned. `local_shell` is here only
for `ls`; the envelope below still keeps it on a short leash.

**`{{var:model}}` / `{{var:provider}}`** resolve to whatever `contenox setup`
configured, which is why this chain works unchanged on Ollama or a hosted
provider.

## 4. Write the envelope

The chain is what the agent does. The envelope is **what it may do** — checked
before every single tool call, by the runtime, not by the model. Save this as
`.contenox/hitl-policy-vault.json`:

```json
{
  "$schema": "https://contenox.com/schema/hitl-policy-v1.schema.json",
  "default_action": "deny",
  "rules": [
    { "tools": "local_fs", "tool": "read_file",  "action": "allow" },

    { "tools": "local_shell", "tool": "local_shell", "action": "allow",
      "when": [ { "key": "command", "op": "command_prefix_allowlist", "value": "ls" } ] },

    { "tools": "local_fs", "tool": "write_file", "action": "allow",
      "when": [ { "key": "path", "op": "glob", "value": "**/vault/**" } ] },

    { "tools": "local_fs", "tool": "write_file", "action": "deny" }
  ],
  "compute": { "maxToolCalls": 40, "maxTokens": 400000, "onExhausted": "finish_stuck" }
}
```

The whole design is in the last two rules, and the **order matters** — the first
rule that matches decides:

1. A `write_file` whose `path` is inside a `vault` folder matches the allow rule
   and runs.
2. Any other `write_file` falls through to the deny rule and is refused.

`"default_action": "deny"` catches everything nobody thought about. A tool you
did not list does not run.

`compute` is the spend ceiling: 40 tool calls and 400k tokens. Crossing it ends
the unit as `stuck` rather than letting it run on.

Check both files before running anything:

```bash
contenox vet .contenox/chain-agent-vaultfiler.json
contenox vet .contenox/hitl-policy-vault.json
```

## 5. Activate the envelope

Writing a policy file does not make it the active one. Name it as the mission
envelope:

```bash
contenox config set default-mission-policy hitl-policy-vault.json
```

```
✓  default-mission-policy = hitl-policy-vault.json  (workspace)
```

`(workspace)` confirms it applies here and not to your other projects. Every
`mission fire` reads this key when you do not pass `--policy` explicitly —
without either set, the fire is refused rather than guessing at an envelope.
See [the envelope](/docs/guide/missions/#the-envelope).

## 6. Run it

```bash
contenox mission fire vaultfiler "File everything in inbox/ into the vault." --wait
```

```
Mission fired at agent "vaultfiler" under envelope "hitl-policy-vault.json".
Intent: File everything in inbox/ into the vault.
Waiting for a terminal status…
Status: landed  (18s)
```

`--wait` blocks until the mission reaches a terminal status, then prints its
report summaries — [`contenox mission show <id>`](/docs/reference/contenox-cli/#contenox-mission)
reads the full record any time after.

```bash
$ ls vault/
Dependency Graph CLI.md
Postgres Connection Pool Exhaustion.md
Serializable Snapshot Isolation.md
```

And one of them:

```markdown
---
source: meeting-notes.txt
tags:
  - postgres
  - pgbouncer
  - database
  - operations
---

## Summary
A meeting with Dana on March 3rd addressed two operational issues: a PostgreSQL
connection pool exhaustion problem due to `pgbouncer` running in session mode,
and a nightly backup job overlapping with the ETL window.

## Details
*   **Meeting Date**: March 3rd.
*   **Issue 1**: Postgres connection pool exhausting under load.
    *   **Root Cause**: `pgbouncer` is in session mode.
    *   **Proposed Solution**: Change `pgbouncer` to transaction mode.
    *   **Ownership**: Dana owns this change, behind a feature flag.
*   **Issue 2**: Nightly backup job overlaps the ETL window.
    *   **Proposed Solution**: Move the backup job to 02:00 UTC.
```

Open the vault in Obsidian. The tags are real tags and the notes are real notes,
because Obsidian reads the folder you just wrote into.

## 7. Prove the fence holds

An agent that behaves is not the same as an agent that is contained. Ask it to
write somewhere it should not be able to:

```bash
contenox mission fire vaultfiler \
  "Write a file named draft.md directly inside the inbox folder with the text hello." --wait
```

```
Status: stuck  (6s)
[blocker] The write_file call for inbox/draft.md was denied by the active
policy. Unable to complete the request as given.
```

```bash
$ ls inbox/
meeting-notes.txt
```

That refusal did not come from the model deciding to behave. It came from the
deny rule, evaluated before the call ran.

The distinction matters more than it looks. When you test this yourself, avoid
asking for something your `system_instruction` already forbids — the model will
refuse for that reason and you will have proved nothing. Aim at a path the
prompt says nothing about, and watch the policy do the work.

## Let your editor check the files

Both files carry a `$schema` line, so any editor that understands JSON Schema —
VS Code does out of the box — gives you completion on every field and flags
mistakes as you type.

This is worth more than it sounds. Writing this tutorial, the first draft of the
envelope had `"version": "v1"`. That is wrong: the field is an integer. The
schema catches it in the editor, before a run, instead of after one.

The schemas are generated from the Go types that load these files, so they
cannot drift from what the runtime accepts:

- [`task-chain.schema.json`](/schema/task-chain.schema.json)
- [`hitl-policy-v1.schema.json`](/schema/hitl-policy-v1.schema.json)

## Where to take it next

- **Run it on a schedule** so the inbox empties itself. See
  [events and triggers](/docs/guide/events/).
- **Ask instead of deny.** Change the final `write_file` rule from `"deny"` to
  `"approve"` and the agent pauses for your yes or no instead of being refused.
  See [HITL policies](/docs/guide/hitl/).
- **Add a second pass** that reads the vault and adds `[[wikilinks]]` between
  related notes — another `chat_completion` task after the first.
- **Keep the inference local.** Nothing here leaves your machine if the model
  does not. See [sovereignty](/docs/guide/sovereignty/).

## When to use the Obsidian MCP server instead

The filesystem approach used here needs nothing installed, works with Obsidian
closed, and is the right shape for batch work like this.

Reach for an Obsidian MCP server when you want the agent to talk to a *running*
Obsidian instead — the currently open note, the app's own search, in-app
commands. Those live behind community plugins that expose Obsidian over MCP.
Register one the same way as any other MCP server, as in the
[Notion recipe](/docs/use-cases/notion-mcp/), and the tools it publishes become
policy-scoped tools like any other. The envelope in this tutorial does not
change shape — only the tool names in the rules do.

## Next

- [Guardrails](/docs/guide/confinement/guardrails/) — the six declarations that scope an agent.
- [Declaring agents](/docs/guide/agents/) — the one-file road, for the next agent that does not need a fence this hard.
- [Writing a chain by hand](/docs/guide/chains/writing-a-chain/) — the chain format in full.
- [HITL policies](/docs/guide/hitl/) — the envelope grammar and every operator.
