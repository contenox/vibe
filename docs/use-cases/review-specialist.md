---
title: The review specialist
description: 'A request to review the git diff got a useless answer. The fix was not a better prompt — it was a third branch in the router, with the write tools taken away.'
---

# The review specialist

I asked the agent to review my working tree:

```
❯ can you review the git diff

· local_shell(git diff)

The git diff shows changes focused on improving the cross-platform build and
installation process, specifically for Windows. The Taskfile.yml now includes
Windows-specific build targets and installation commands, and a new
scripts/dev-install.ps1 PowerShell script handles copying the executable and
updating the user's PATH on Windows.

My review of the git diff is complete.
```

That is not a review. It is a restatement of the diff with a sentence claiming completion on the end. Nothing was read beyond the diff itself, no claim was checked, and the one thing a review exists to produce — a defect, with the case that triggers it — is absent.

The tempting fix is to write a better prompt: *"review this diff, look for bugs, check the surrounding code, cite file:line, don't just summarise"*. That works once, for the person who remembers to type it. It does not survive the next session, the next user, or the next surface.

## Why it happened

The chain's router knew two labels:

```
Use 'coding_change' for requests to create, edit, migrate, refactor, fix, test,
build, or otherwise change code/files.
Use 'general' for questions, explanations, brainstorming, setup help, or
anything that should use the normal assistant loop.
```

"Review the git diff" is not a change, so it classified as `general` and went to the assistant loop — which has no review contract at all. The loop behaved correctly: it answered a question, called one tool, and stopped. Nobody had ever told it what a review is.

The failure was not in the model and not in the phrasing. It was a gap in the configuration, and it was invisible: the answer looked like an answer.

## The fix

A third branch. One sentence added to the classifier:

```
Use 'review_change' for requests to review, audit, critique, or assess code that
already exists — a diff, a commit, a file, a branch — where the answer is an
assessment and nothing is to be modified. A request to review AND then fix is
'coding_change', not 'review_change'.
```

one branch above the default:

```json
{ "operator": "equals", "when": "review_change", "goto": "review_chat" }
```

and a loop that carries the method. The parts that matter:

**It cannot write without a human seeing the call first.** Not "is asked not to":

```json
"tools": ["*"],
"hide_tools": ["local_fs.write_file", "local_fs.edit_file", "local_fs.sed"]
```

paired with an `approve` rule on `local_shell` in this loop's HITL policy. Git now goes through the shell, and the shell has no separate read-only tool to grant instead — so the boundary moves from the tool schema to the approval gate: every shell call this loop makes, `git diff` included, is shown to a human before it runs. `hide_tools` is enforced where calls execute, so a model that asks for `write_file` anyway does not get to run it. The same list is repeated on the paired `execute_tool_calls` task — a tool withheld only where the model is *asked* would still run where calls are *executed*.

**It has a procedure**, not an exhortation — establish the change with `git diff`/`git status`/`git show` via `local_shell`, then read the full current text of every file the diff touches, then follow the callers of anything whose contract moved.

**It defines what a finding is:** a defect you can state a failure for — the input or state that triggers it, and the wrong result — ordered by consequence, cited to `file:line` from tool output in this turn. And what a finding is not: style the repo does not enforce, naming you would have chosen differently, defensive checks for states the types already exclude, restatements of what the diff does.

**It has an evidence bar.** The first version of this loop answered *"The change is sound. No files skipped"* after two tool rounds. Technically it obeyed the output contract; practically it had reached a verdict from the diff alone. The instruction now says a verdict may not be reached until the full current text of every touched file has been read, and that the closing line names what was read rather than what was skipped.

**It gets a bigger budget than the coding loop** — reading every touched file costs more tool rounds than editing one.

## The result

The same sentence, unchanged, now takes a different path. From `~/.contenox/telemetry.log`:

```
classify_request → review_chat → review_tools → review_chat → review_tools → review_chat
```

`coding_chat` never runs. No shell call and no write happen without a human seeing them first. The user typed exactly what they typed before.

## What this is really about

The gap was not the model's and not the user's. It was the workflow's, and it was fixed where workflows are written — [in the router](/docs/guide/chains/routing/), once, for every session and every surface that shares the chain.

That is the trade contenox is built around. You can spend the effort every time, in the prompt, where it is invisible and unversioned and only as good as what the user remembered to say. Or you can spend it once, in a file you can read, diff, review and ship — and let people go on writing ordinary sentences.

Nothing about this is specific to code. The same shape applies wherever a request's *kind* determines the method it deserves and the capabilities it may use: a support tier, a moderation gate, an extraction contract, a regulated data path.
