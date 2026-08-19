---
title: AGENTS.md
description: Project-level instructions and context that load automatically into every session.
order: 6
---

# AGENTS.md

Contenox follows the [AGENTS.md community standard](https://agents.md) — a `README` for AI coding agents. Drop an `AGENTS.md` file at your project root (or any parent directory) and it loads automatically into every new chat session as a system message.

The same file works across [60+ tools](https://agents.md) including Codex, Aider, Cursor, Gemini CLI, Jules, Devin, and more. Write it once, every agent reads it.

This page is the mechanism. For the argument — what belongs in the file, what breaks without it, and why a stale one is worse than none — see [Why a project needs an AGENTS.md](/docs/guide/agents-md-why/).

## How it loads

When `contenox chat` opens a conversation, it walks up from the current working directory to find the closest `AGENTS.md`. If found, it's prepended to the chat history as a single `system` message — once per session, not once per turn — and persisted alongside the conversation.

> **Not yet everywhere.** Today only `contenox chat` loads it. The editor surfaces (`contenox acp`, `contenox acpx`), `beam`, and the unattended shapes (`contenox run`, `contenox mission fire`) start their sessions without it, so a project's `AGENTS.md` does not reach an agent driven from an editor or from beam. This is a gap, not a design: those surfaces open their session on a client-supplied working directory, which is where the walk has to start for them.

Because it lands in chat history (not the system prompt), it's:

- **Cached** by providers on subsequent turns — no re-render cost
- **Persisted** in the message store — survives restarts, visible in `sqlite3 ~/.contenox/local.db`
- **Reference material**, not unconditional rules — the model treats it as project context to consult, not directives to obey blindly

## Closest-wins precedence

If you have nested `AGENTS.md` files (monorepo with per-package context), the one closest to your current working directory wins. The loader walks up from `cwd` toward the filesystem root and stops at the first hit.

For very large monorepos, this means each package can ship tailored instructions without polluting the root file.

## Cap

Files larger than 64 KiB are truncated with a marker. Keep AGENTS.md focused on the things the agent *needs* to know, not full architectural docs — link to longer docs from there.

## Staleness

`AGENTS.md` is read at session start and persists in history. Updating the file mid-session does *not* update the loaded copy — start a new session (`contenox session new`) to pick up changes. This matches how Claude Code's `CLAUDE.md` and most other agents handle this.

## What to put in it

The spec is open — any markdown — but useful sections include:

```markdown
# Project name

## Setup commands
- Install deps: `make deps`
- Run tests: `make test`
- Build binary: `make build`

## Code style
- Go 1.25, no panics in library code
- All tools take `libdb.DBManager` at construction
- Tests use `t.TempDir()` + `libdb.NewSQLiteDBManager` for SQLite

## Project conventions
- Mutating local_fs operations require a prior `read_file` (read-before-write contract)
- HITL is on by default for write_file, edit_file, sed, and local_shell

## Don't do
- Never run `git push --force` unprompted
- Never edit files under `vendor/`
```

## Verifying it loaded

After a `contenox chat` turn, the AGENTS.md content is the first message in the persisted history. Print the head of the active session to check:

```bash
contenox session show --head 1
```

The first message should be a `system` message whose content is your `AGENTS.md` (plus a short wrapper). If it isn't there, the loader didn't find an `AGENTS.md` in the working tree — confirm the file exists at or above your current directory, and that the session was opened by `contenox chat` rather than by one of the surfaces named above.

(The active session id is stored per-workspace in the SQLite KV store as a JSON-quoted value, so a hand-written `sqlite3` join against the `messages` table is fiddly to get right — `contenox session show` resolves the active session for you.)

## Per-tool conventions

Other AGENTS.md adopters expose configuration knobs (Aider's `read: AGENTS.md`, Gemini CLI's `context.fileName`). Contenox doesn't need configuration — the loader is on by default and follows the spec verbatim. To disable, don't ship an `AGENTS.md` in the project tree.
