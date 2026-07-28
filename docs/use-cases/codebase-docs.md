---
description: Point contenox at your source tree and get living architecture guides, API references, and onboarding docs — without leaving the terminal.
---

# Codebase Documentation

Read your source code locally and produce living documentation — architecture guides, API references, onboarding wikis — without leaving the terminal.

## Prerequisites

```bash
# Enable the local filesystem MCP server (one-time)
contenox mcp add filesystem --transport stdio \
  --command npx --args "-y,@modelcontextprotocol/server-filesystem,$PWD"

# Optional: add Notion to publish the result directly
contenox mcp add notion --transport http --url https://mcp.notion.com/mcp --auth-type oauth
contenox mcp auth notion
```

---

## Recipe 1: Generate an architecture guide

```bash
contenox --shell "Read the Go source files under ./internal and ./taskengine. \
  Write a markdown architecture guide covering: key packages, main data flows, \
  and the relationship between runtimestate, llmrepo, and taskengine."
```

The model uses `list_dir` and `read_file` to traverse the repo, synthesises the architecture, and returns a full markdown document — grounded in actual code, not guesses.

---

## Recipe 2: Publish the guide straight to Notion

```bash
contenox run --shell "Read the Go source files under ./internal and ./taskengine. \
  Write an architecture guide as markdown, \
  then use the Notion MCP tools to create a page titled 'Architecture: $(basename $PWD)' with that content."
```

The model reads the files, writes the guide, then calls Notion's `create_page` tool — one step, zero copy-pasting.

---

## Recipe 3: Onboarding wiki for a new hire

```bash
contenox run --shell \
  "Read ./README.md, ./docs/, and the go.mod file. \
   Write a 'Getting Started' onboarding guide covering: \
   what this project does, how to run it locally, key concepts, and first tasks for a new contributor. \
   Then use the Notion MCP tools to create a page called 'Onboarding: $(basename $PWD)'."
```

---

## Recipe 4: Keep docs in sync after a refactor

```bash
git diff HEAD~1 | contenox \
  "This diff is a recent refactor. Read the current source under ./internal too (use shell tools). \
   Update the existing architecture notes to reflect these changes. \
   Return only the updated sections."
```

Pipe the diff in, let the model compare it against the live source, and get targeted doc patches back.

---

## Recipe 5: Generate a module-level API reference

```bash
contenox --shell \
  "List all exported functions in ./taskengine/*.go. \
   For each, read its GoDoc comment and signature. \
   Produce a markdown API reference table: Function | Signature | Description."
```

---

## How it works

`--shell` enables the local shell tools. The default run chain also exposes registered MCP servers, so a single `contenox run --shell` invocation can combine local reads with Notion writes.

```
Local files → model reads & reasons → Notion page created
      ↑                                        ↑
 local_fs/local_shell tools            notion MCP tools
```

The task engine handles the full tool-call loop automatically. The model can interleave reads (filesystem) and writes (Notion) in a single run.

> [!TIP]
> Add `--trace` to watch every file read and Notion API call in real time — useful for debugging large repos where the model might need to follow import chains.

> [!NOTE]
> For very large codebases, scope the read to specific packages or pass a file list via stdin to stay within the model's context window.
