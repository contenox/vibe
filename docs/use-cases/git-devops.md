---
title: Git & DevOps recipes
description: Commit messages, PR reviews and test-failure triage as declared agents fired by contenox run — an exit code to branch on, and a durable record of every run.
---

# Git & DevOps recipes

The work around a change — writing the commit message, reviewing the diff,
reading the test output — is repetitive, and every one of those jobs is a small
agent with a narrow tool list. Declare it once, then fire it from a shell alias,
a git hook, or a CI step.

## Prerequisites

- `contenox init` once in the repository, and a model configured — see
  [Quickstart](/docs/guide/quickstart/).
- An **envelope**. [`contenox run`](/docs/reference/contenox-cli/#contenox-run)
  fires a [mission](/docs/guide/missions/), and a mission that names no envelope
  is refused:

  ```bash
  contenox config set default-mission-policy hitl-policy-default.json
  ```

---

## Commit messages from your diff

`.contenox/agents/commit-msg.md`:

```markdown
---
name: commit-msg
description: Writes a Conventional Commits message for the staged changes
tools: Read, git.git_status, git.git_diff, git.git_log
---

You write one commit message for the change that is staged right now.

Establish the change before describing it: git_status for what is staged,
git_diff for the staged text, git_log for the subject conventions this
repository already uses. Read the current file around a hunk when the hunk
alone does not say what changed.

FORMAT: Conventional Commits. A `type(scope): subject` line under 72
characters, in the imperative, then a blank line, then one bullet per distinct
change. Name what changed and why it changed; do not restate the diff line by
line, and do not pad the body to look thorough.

A change you cannot explain from what you read gets a bullet saying so, not a
guess.

OUTPUT: the message, and nothing around it. No preamble, no offer to revise.
```

Every tool on that list is read-only. This agent describes the change; it cannot
stage, commit or amend it — and that is settled by the declaration rather than by
asking the model nicely.

```bash
git add -p
contenox run commit-msg "write the message for what is staged"
```

Stdout carries the agent's final report and nothing else — progress, the mission
id and the outcome all go to stderr — so a pipe reads clean:

```bash
git commit -e -m "$(contenox run commit-msg 'write the message for what is staged')"
```

`-e` is deliberate. Stdout is the *report* the agent filed, which is as clean as
the output contract in the declaration makes it and no cleaner. Open it in the
editor, read it against your own diff, and commit what you meant to commit.

---

## Review a change before it goes out

For one file, `reviewer` ships preseeded and there is nothing to declare:

```bash
contenox run reviewer "review ./internal/payments/retry.go"
```

A whole branch is a different job, and the difference is the reason it gets its
own declaration. An agent handed a diff will happily summarise it and call that
a review — the long version of that story is
[the review specialist](/docs/use-cases/review-specialist/). What stops it is an
evidence bar in the prompt and a tool list with no way to edit anything.

`.contenox/agents/branch-review.md`:

```markdown
---
name: branch-review
description: Reviews the commits on this branch for correctness problems, and modifies nothing
tools: Read, Bash, git.git_status, git.git_diff, git.git_log, git.git_show, git.git_blame
---

You review a change that has not landed. You modify nothing: the write tools
are withheld from this agent, not merely discouraged.

PROCEDURE: establish the change first — git_diff for the working tree,
git_status for what is staged, git_show for a commit, git_log for the history
around it. Then read the current full text of every file the change touches
before judging any of it: a hunk is correct or incorrect only against the code
around it. Follow the callers of anything whose signature or contract moved,
with grep and find through the shell.

A FINDING is a defect you can state a failure for: the input or state that
triggers it, and the wrong result it produces. Order them by consequence — data
loss, then wrong behaviour, then a contract broken for callers, then resource
and concurrency faults, then tests that assert nothing. Cite file:line, and only
from tool output in this turn.

NOT FINDINGS: style the repository does not enforce, naming you would have
chosen differently, defensive checks for states the types already exclude, and
restatements of what the diff does.

EVIDENCE BAR: you may not conclude anything — including that the change is
sound — before you have read the full current text of every file it touches.

OUTPUT: the findings, most severe first, then one line naming the files you
read. If you found nothing, say so in one line and list nothing.
```

```bash
gh pr checkout 42
contenox run branch-review "review this branch against main"
```

Not one tool on that list can change a file. The reviewer that cannot edit is a
configuration fact, checkable by reading the frontmatter, and it holds in a
pipeline exactly as it holds at your keyboard.

---

## Triage a failing test run

The default `run` declaration has the shell, so it can run the suite itself:

```bash
contenox run "run go test ./... and report which tests failed and why, citing the file and line each failure points at"
```

That is one mission: it runs the command, reads the source behind each failure,
and reports. Compare with the piped form, which only reads the output you already
have:

```bash
go test ./... 2>&1 | contenox "what failed here and why?"
```

Use the pipe when the output is in front of you and you want it explained. Use
the run when you want something looked into.

---

## Aliases

```bash
# Commit message for what is staged
alias cx-commit='contenox run commit-msg "write the message for what is staged"'

# Review the current branch
alias cx-review='contenox run branch-review "review this branch against main"'

# Explain a command's output
alias cx-why='contenox "explain this output and flag anything concerning"'
```

The last one is the piped form: `df -h | cx-why`, `ps aux | cx-why`.

---

## In a pipeline

`contenox run` exits 0 when the mission landed and non-zero when it derailed,
got stuck, was abandoned, or the wait timed out. A CI step branches on that
without parsing a word of the report:

```bash
contenox run branch-review "review this branch against origin/main" \
  --policy hitl-policy-strict.json --timeout 10m \
  || echo "review did not complete — see: contenox mission list"
```

Two things behave differently here than they do at a keyboard, and both are worth
knowing before you schedule anything.

**A gated call has nobody to ask.** There is no terminal in front of a scripted
run, so a call the envelope gates becomes a
[durable ask](/docs/guide/hitl/#what-a-parked-approval-looks-like) and the run
parks until someone answers it with `contenox approvals respond` — or until
`--timeout` (default 30m) tears it down with a non-zero exit. What a scripted run
may do unattended is bounded by its envelope, not by anyone watching it.

**The run is not disposable.** Every `contenox run` is a mission, and its record,
its reports and its plan survive the pipeline that threw its stdout away:

```bash
contenox mission list                    # newest first: id, agent, envelope, status
contenox mission reports <mission-id>    # every report in full
```

That is what makes a scheduled agent auditable after the fact: the envelope it
ran under is written into the record at fire time, so the bounds a run worked
inside are a fact you can read back, not a setting that may have changed since.
