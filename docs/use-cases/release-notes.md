---
title: Automated release notes
description: A declared agent reads the commit range with the git tools and writes RELEASE_NOTES.md; contenox run fires it from CI and exits 0 when the file landed.
---

# Automated release notes

Turn a tag range into grouped release notes from a Makefile or a CI step, with
nobody at the keyboard.

## Prerequisites

- `contenox init` once in the repository, and a model configured — see
  [Quickstart](/docs/guide/quickstart/).
- An **envelope**. [`contenox run`](/docs/reference/contenox-cli/#contenox-run)
  fires a [mission](/docs/guide/missions/), and a mission that names no envelope
  is refused rather than guessing:

  ```bash
  contenox config set default-mission-policy hitl-policy-default.json
  ```

  Or name one per invocation with `--policy`.

---

## The agent

`.contenox/agents/release-notes.md`:

```markdown
---
name: release-notes
description: Writes RELEASE_NOTES.md for one commit range, grouped by kind
tools: Read, Write, git.git_log, git.git_show
---

You write release notes for one commit range and nothing else.

Read the range with git_log. Group the commits under `## Features`,
`## Bug Fixes`, `## Improvements`, `## Documentation`, in that order, and omit
any section that would be empty. One line per commit, rewritten to say what
changed for someone using the software rather than what the patch touched.

Drop merge commits and version bumps. When a subject is too terse to classify,
read the commit with git_show before deciding — never infer a category from a
subject you did not verify.

Write the result to RELEASE_NOTES.md, replacing whatever is there. Then report
the range you read, the file you wrote, and how many commits landed in each
section. Do not paste the notes back into the report.
```

Four tools, all of them named. An omitted `tools:` line would inherit every tool
on the machine, including the shell — a job that reads the log and writes one
file has no use for that.

No build step. The next run picks the file up:

```bash
contenox agent list
```

---

## Fire it

```bash
contenox run release-notes "notes for v0.9.0..HEAD"
```

Stdout carries the agent's final report; stderr carries the progress and the
mission id. The exit status is 0 when the mission landed, so a pipeline branches
on it without parsing anything:

```bash
contenox run release-notes "notes for $PREV_TAG..HEAD" \
  && gh release create "$NEW_TAG" --notes-file RELEASE_NOTES.md
```

**Stdout is the report, not the artifact.** This is the part worth reading
twice. `contenox run` prints what the agent did — *"wrote RELEASE_NOTES.md, 34
commits, 4 sections"* — not the document itself. So the agent writes the file
and the caller reads the exit code. Redirecting stdout into `RELEASE_NOTES.md`
would capture the account of the work instead of the work.

## The piped form

When you want a filter rather than a job — no declaration of your own, nothing
written unless the task asks for it — pipe the commits in and read the answer
back:

```bash
git log --oneline "$PREV_TAG"..HEAD | contenox "group these commits under ## Features, ## Bug Fixes, ## Improvements, ## Documentation; omit empty sections; no preamble"
```

The shell produces the commit list and contenox only formats it. Nothing runs
`git` on the agent's behalf here, because the model never needs to: the commits
arrive as text.

---

## In CI

```bash
contenox run release-notes "notes for $PREV_TAG..HEAD" \
  --policy hitl-policy-release.json --timeout 10m
```

Two flags carry the weight:

- **`--policy`** names the envelope that bounds the unit. Write it for this job
  ([how](/docs/use-cases/authored-approval/)): an agent that reads the log and
  writes one file needs an `allow` rule for that write and nothing else. An
  envelope that only *approves* the write is the wrong one here, for the reason
  in the next point.
- **`--timeout`** (default 30m) caps the wait. There is no terminal in front of
  a scripted run, so a gated call has nobody to ask: it becomes a
  [durable ask](/docs/guide/hitl/#the-life-of-an-ask), the run
  waits on it, and the wait eventually times out with a non-zero exit. When a CI run
  times out this way, the fix is in the envelope — the call the agent needed was
  gated, not allowed — and not in the prompt.

The mission record outlives the pipeline that discarded its output:

```bash
contenox mission list
contenox mission reports <mission-id>
```

---

## Variations

- **A stronger model for the grouping**, without touching the declaration:
  ```bash
  contenox run release-notes "notes for $PREV_TAG..HEAD" --model gpt-5-mini --provider openai
  ```
- **First release, no previous tag** — give the agent the root commit instead:
  `"notes for <root-sha>..HEAD"`.
- **Per-component notes** — one run per path, each writing its own file. Every
  run is a separate mission with its own record.
