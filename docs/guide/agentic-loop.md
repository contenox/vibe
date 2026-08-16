---
title: The agentic loop
description: The ReAct loop as contenox implements it — an authored task graph you copy and trim, not vendor plumbing you configure.
---

# The agentic loop

In contenox the ReAct loop — model reasons, calls a tool, observes, reasons again — is not hidden vendor plumbing. It is an authored task graph: a `chat_completion` task with tools, a branch on whether the model called one, an `execute_tool_calls` task, and an edge back. Every decision is a visible JSON key; every loop is bounded by a budget you can read. The shipped chains stage that loop deliberately: a main loop that works, a recovery loop with a fresh instruction and a smaller budget, and a terminal task with `"tools": []` that can only report. Each stage that follows a failure gets fewer powers and a clearer mandate — the graph never lets confusion default into unattended mutation.

You do not have to author one to get one. [Declare an agent](/docs/guide/agents/) and contenox generates the chain behind it, staged the way the shipped ones are. This page is for reading that chain, and for the case where you write your own: a branch, a different model per step, a recovery path, a declared point where a human stands.

It maps the loop as the shipped chains actually implement it, then shows how to derive your own. Authoring basics — tasks, handlers, transitions — are covered in [Writing a chain by hand](/docs/guide/first-chain/); this page is about loop engineering: the topology, the budgets, and the doctrine for adapting it.

## Anatomy of one turn

One round of the agentic loop is two tasks and three branches:

1. A `chat_completion` task sends the history to the model. Its eval is a fixed control token: `"tool_call"` when the model requested tools, `"executed"` when it replied with text.
2. The `transition.branches` are checked top to bottom. An `equals` branch on `"tool_call"` routes to the execute task; the `default` branch routes to `end`.
3. The `execute_tool_calls` task runs the calls from the chat task (named by `input_var`), appends the results to the chat history, and its own `default` branch is the back-edge into the chat task.
4. An `edge_traversed_at_least` branch, placed *ahead* of the `tool_call` branch, bounds the loop: it reads the engine's edge counter, not the task output, and fires once the chat→execute edge has been traversed N times this run.

`chain-planner-default.json` — the shipped mission planner — does exactly this. Its main task's transition, verbatim:

```json
"transition": {
  "on_failure": "plan_recovery",
  "branches": [
    {
      "operator": "edge_traversed_at_least",
      "edge": "plan_loop->plan_tools",
      "when": "16",
      "goto": "plan_recovery"
    },
    {
      "operator": "equals",
      "when": "tool_call",
      "goto": "plan_tools"
    },
    {
      "operator": "default",
      "when": "",
      "goto": "end"
    }
  ]
}
```

And the execute step it routes to, trimmed to its moving parts (the full task also repeats the tool allowlist) — the whole back-edge is one `default` branch:

```json
{
  "id": "plan_tools",
  "handler": "execute_tool_calls",
  "input_var": "plan_loop",
  "transition": {
    "branches": [
      { "operator": "default", "when": "", "goto": "plan_loop" }
    ]
  }
}
```

Read in order: budget first, tool loop second, done last. The model keeps the loop alive by calling tools; the graph decides when that stops.

> **Note:** you cannot branch on failure. When a task errors, the engine routes to `on_failure` *before* any branch is evaluated — a `{ "when": "failed" }` branch can never fire. In the loop above, `"on_failure": "recovery_run"` is what catches a broken turn; the branches only ever see `"tool_call"` or `"executed"`. See [Transitions & branching](/docs/specification/transitions/).

## The staged production shape

An agent declared as a directory tree (see [Declaring agents](/docs/guide/agents/)) wraps that two-task loop in the same production staging, expressed as files rather than as JSON stages:

- **Entry classification** is the tree's root `agent.md` — a router whose branches are its subdirectory names. `contenox` appends each branch's `description` to the classifier prompt, so the model is told exactly which labels are valid, and a `default:` branch says where an unclassified request goes. A flat, single-file declaration has no classifier — the one prompt is its own classification.
- **The main bounded loop** is a leaf's `agent.md` — chat task, execute task, back-edge, budget. The budget branch routes to that leaf's recovery, not straight to `end`: spending the budget is treated as a signal the loop is stuck, not as a place to stop mid-thought.
- **The recovery loop, with its own budget**, is the leaf's `recovery.md` — present only where a second attempt is worth having; a branch that should simply give up omits the file. It picks up the *same* chat history under a fresh instruction that says plainly: the main loop got stuck, continue from the actual history, do not restart. The prompt can show its own live spend with `{{rounds_used}}` / `{{main_rounds}}` / `{{recovery_rounds_used}}` / `{{recovery_rounds}}` — `contenox` resolves those as live edge counts over that leaf's own loop, so nothing names a task id or hardcodes a number.
- **`failure.md`: report the true state.** One per tree, reached when every branch has given up — it cannot act, and its instruction is the honesty contract: say what was attempted, what was actually done, where it got stuck, and what would unblock it. Do not pretend the work is complete.
- **The bounds around every stage** — token limits, retry policy, tool allowlists and shell command policy — live in [`agents.toml`](/docs/reference/agents-config/) beside the declarations, not in the declarations themselves. A value set at the top applies to every agent; nest it under `[agents.<name>.chain]` to override one.

**Where HITL sits.** The loop decides *what the model may attempt*; the active [HITL policy](/docs/guide/hitl/) decides *what actually runs unattended*. An `approve` rule pauses a tool call mid-loop — the `execute_tool_calls` task waits, a human answers, the loop resumes. That envelope is a separate authored file, so the same chain can run gated in one workspace and free in another.

A hand-authored chain follows the same skeleton without any of that machinery. `chain-planner-default.json` — the shipped mission planner, and the concrete example above — proves the topology is the constant and everything else is configuration: chat → tools → back-edge, a budget branch, a recovery loop with its own smaller budget (16 main / 6 recovery tool-call rounds), and no terminal task at all — its recovery routes straight to `end` because the mission tools themselves are the reporting channel. Each `chat_completion` task also carries a `retry_policy` for transient provider errors (`max_attempts: 4`, `initial_backoff: "1s"`, `max_backoff: "30s"`, `jitter: 0.25`, `rate_limit_min_wait: "10s"`), and the chain-level `token_limit` (131072 here) bounds the context, with `"shift": true` sliding the window instead of erroring. The [chains development doc](/docs/development/chains/) frames why: the chain is the reviewed execution contract around the loop.

## Creating one: copy, then delete

For most agents, don't write a chain by hand at all — [declare it](/docs/guide/agents/) as a Markdown file and `contenox` generates the chain behind it. This section is for what a declaration cannot say: a branch, a different model per step, a point where a human is required. There the doctrine is the same one, applied to a file instead of a directory tree: **do not invent a loop topology. Copy the leanest real chain you have and strip or extend it.** `chain-planner-default.json` under `~/.contenox/system/` is the leanest currently-shipped example — four tasks, no classifier, one persona — and already encodes the answers to the questions a hand-rolled loop gets wrong: where the budget branch goes, what `on_failure` catches, why the recovery loop has its own smaller budget.

Walk one adaptation end-to-end — a narrow diff-review loop, single tool, attended use:

1. **Delete the stages you don't need.** Remove `plan_recovery` and `plan_recovery_tools` — two tasks gone.
2. **Remove `"on_failure": "plan_recovery"`** from the main task. An error now terminates the run, which is acceptable when a human is watching the output.
3. **Repoint the budget branch** from `plan_recovery` to `end`, and lower `"when"` from `"16"` to `"6"` — a review should not need sixteen tool rounds.
4. **Narrow the tools.** `"tools": ["mission"]` becomes `["local_shell"]` on *both* tasks, plus a `tools_policies` block scoping `local_shell` to `_allowed_commands: "git,go"` and `_denied_commands: "sudo,su,dd,mkfs,fdisk,parted,shred"`. Keep the chat task's grant and the execute task's grant identical — that symmetry is what makes the contract reviewable in one read.
5. **Rename the tasks** (`plan_loop` → `review`, `plan_tools` → `review_tools`) and rewrite `system_instruction` to the reviewer persona. Drop `retry_policy` and `max_tokens` — this is the minimal set.

> **Note:** the `edge` string in `edge_traversed_at_least` spells the literal task ids: `"review->review_tools"`. Renaming a task means renaming every edge that mentions it — `contenox vet` catches the dangling reference if you miss one.

The result is the minimal agentic loop — derived from the real template, complete and valid:

```json
{
  "id": "chain-agent-diffreview",
  "description": "Bounded single-tool review loop derived from chain-planner-default.json: run read-only git commands via local_shell, answer once.",
  "tasks": [
    {
      "id": "review",
      "handler": "chat_completion",
      "system_instruction": "You are a code reviewer. The input is a diff. Inspect the repository with git when needed, then give one concise review: correctness first, style second. State facts about the code only from tool output in this turn.",
      "execute_config": {
        "model": "{{var:model}}",
        "provider": "{{var:provider}}",
        "shift": true,
        "tools": ["local_shell"],
        "tools_policies": {
          "local_shell": {
            "_allowed_commands": "git,go",
            "_denied_commands": "sudo,su,dd,mkfs,fdisk,parted,shred"
          }
        }
      },
      "transition": {
        "branches": [
          {
            "operator": "edge_traversed_at_least",
            "edge": "review->review_tools",
            "when": "6",
            "goto": "end"
          },
          { "operator": "equals", "when": "tool_call", "goto": "review_tools" },
          { "operator": "default", "when": "", "goto": "end" }
        ]
      }
    },
    {
      "id": "review_tools",
      "handler": "execute_tool_calls",
      "input_var": "review",
      "execute_config": {
        "tools": ["local_shell"],
        "tools_policies": {
          "local_shell": {
            "_allowed_commands": "git,go",
            "_denied_commands": "sudo,su,dd,mkfs,fdisk,parted,shred"
          }
        }
      },
      "transition": {
        "branches": [{ "operator": "default", "when": "", "goto": "review" }]
      }
    }
  ],
  "token_limit": 65536
}
```

`contenox vet ./chain-agent-diffreview.json` must pass it — the linter checks handler signatures, dataflow across every `goto` and `on_failure` edge, and branches that can never fire. Drop it in `.contenox/` and it's discovered as agent `chain-agent-diffreview` ([chain files: naming, roles, and resolution](/docs/guide/chain-naming/)); fire it, and let its own `local_shell` run the `git diff` the system prompt asks for:

```bash
contenox mission fire chain-agent-diffreview "review the current diff" --wait
```

## Minimal vs production

What the minimal loop gave up, and when that trade is sound:

| | Minimal loop | Production loop (the shipped template) |
|---|---|---|
| Budget exhaustion | routes to `end` — possibly mid-thought, on a tool-call turn | routes to a recovery loop, then `summarise_failure` reports the true state |
| Task errors | terminate the run | `on_failure` cascades: main → recovery → summary |
| Transient provider errors | kill the run | `retry_policy` with backoff, jitter, rate-limit wait |
| Entry | one persona for every input | `route` classifier picks the loop and its budget |
| Fine for | attended runs, personal tools, one-shots you watch | unattended agentic workflows, missions, CI |

The production set is the template you copied — so "upgrading" the minimal loop is deleting less next time. Restore the recovery stage before you put the chain in front of anything unattended: an agent that cannot report its own failure will report success instead.

## Next

- [Request routing](/docs/guide/request-routing/) — the layer above this one: how a `route` task picks which loop runs, and what a specialist carries beyond its prompt
- [Writing a chain by hand](/docs/guide/first-chain/) — authoring basics: tasks, prompts, models, policies
- [Chain files: naming, roles, and resolution](/docs/guide/chain-naming/) — where chain files live and what the `agent` role means
- [Transitions & branching](/docs/specification/transitions/) and [Handlers](/docs/specification/handlers/) — the full operator and handler reference
- [HITL policies](/docs/guide/hitl/) — the approval envelope around the loop
- [Chains](/docs/development/chains/) — the authored-contract framing behind all of this
