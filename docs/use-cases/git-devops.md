---
description: Pipe your diff or log into contenox run and get commit messages, PR summaries, and reviews back — no setup, no sessions.
---

# Git & DevOps Recipes

`contenox run` shines as a composable shell tool. Pipe in data from your terminal, get AI-powered output back — no setup, no sessions.

---

## Generate a commit message from your diff

Let the model read your staged or unstaged diff and write a [Conventional Commits](https://www.conventionalcommits.org/)-style message.

**Pipe the diff — the model reads it and writes the message:**

```bash
$ git diff | contenox run "suggest a commit message for the following git diff"
Thinking...
Based on the diff provided, you are performing a significant refactor of the CLI commands:
·  Renaming 'exec' to 'run' (for stateless execution).
·  Renaming 'run' to 'chat' (for stateful chat sessions).
·  Updating command structures, documentation, and config defaults.

refactor: rename CLI subcommands and update config defaults

- Rename 'exec' subcommand to 'run' for stateless task execution
- Rename 'run' subcommand to 'chat' for stateful chat sessions
- Update default context length to 131072
- Expand allowed local_shell commands
- Update internal CLI routing, documentation, and website references
- Add support for string-to-chat input coercion in task engine
```

**Or let the model run `git diff` itself:**

```bash
contenox run --shell "suggest a commit message for the current git diff"
```

**Pipe only staged changes before committing:**

```bash
git diff --cached | contenox run "suggest a commit message for these staged changes"
```

**With a custom system instruction via your own chain:**

```bash
git diff | contenox run --chain .contenox/commit-msg-chain.json "write a commit message"
```

> **Tip:** Pipe `git diff --cached` to cover only staged changes before committing.

---

## Review a pull request diff

```bash
git diff main...HEAD | contenox run "review this pull request diff: highlight bugs, security issues, and style problems"
```

Or fetch a PR diff from GitHub CLI and pipe it:

```bash
gh pr diff 42 | contenox run "review this PR diff and summarize the main concerns"
```

---

## Summarize a build or test log

```bash
cat build.log | contenox run "summarize this build log, highlight errors and warnings"
go test ./... 2>&1 | contenox run "what failed in this test output and why?"
```

---

## Explain a command's output

```bash
df -h | contenox run "explain this disk usage output and flag anything concerning"
ps aux | contenox run "what are the top resource consumers in this process list?"
```

---

## Alias examples

Add these to your `.bashrc` / `.zshrc` for one-liners:

```bash
# Commit message from staged changes
alias cx-commit='git diff --cached | contenox run "write a conventional commit message for this diff"'

# PR review
alias cx-review='git diff main...HEAD | contenox run "review this diff: flag bugs and style issues"'

# Summarize test failures
alias cx-test='go test ./... 2>&1 | contenox run "summarize test failures"'
```

---

## How it works

`contenox run` executes a **stateless chain** — it takes your input, runs it through the model (with optional tools like `local_shell`), and prints the result. Nothing is saved to history.

- Use `--shell` to let the model call tools like `git`, `cat`, `ls`
- Pipe data via stdin to feed context without needing shell access
- Use `--chain` to swap in a custom chain for specialized tasks
- Use `--input-type chat` to pass structured conversation history

See [`contenox run` reference →](/docs/reference/contenox-cli#contenox-run)
