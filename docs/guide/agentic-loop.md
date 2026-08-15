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

`chain-agent-run.json` — the stateless `contenox run` loop — does exactly this. Its main task's transition, verbatim:

```json
"transition": {
  "on_failure": "recovery_run",
  "branches": [
    {
      "operator": "edge_traversed_at_least",
      "edge": "contenox_run->run_tools",
      "when": "10",
      "goto": "recovery_run"
    },
    {
      "operator": "equals",
      "when": "tool_call",
      "goto": "run_tools"
    },
    {
      "operator": "default",
      "when": "",
      "goto": "end"
    }
  ]
}
```

And the execute step it routes to, trimmed to its moving parts (the full task also repeats the tool allowlist and `tools_policies`) — the whole back-edge is one `default` branch:

```json
{
  "id": "run_tools",
  "handler": "execute_tool_calls",
  "input_var": "contenox_run",
  "transition": {
    "branches": [
      { "operator": "default", "when": "", "goto": "contenox_run" }
    ]
  }
}
```

Read in order: budget first, tool loop second, done last. The model keeps the loop alive by calling tools; the graph decides when that stops.

> **Note:** you cannot branch on failure. When a task errors, the engine routes to `on_failure` *before* any branch is evaluated — a `{ "when": "failed" }` branch can never fire. In the loop above, `"on_failure": "recovery_run"` is what catches a broken turn; the branches only ever see `"tool_call"` or `"executed"`. See [Transitions & branching](/docs/specification/transitions/).

## The staged production shape

The shipped `chain-agent-*.json` files all wrap that two-task loop in the same production staging:

| Chain | Entry | Main loop budget | Recovery budget | Terminal |
|-------|-------|-----------------|-----------------|----------|
| `chain-agent-run.json` | none — straight into the loop | 10 | 10 | `summarise_failure` |
| `chain-agent-contenox.json` | `classify_request` (`route`) | 12 coding / 10 general | 8 | `summarise_failure` |
| `chain-agent-acp.json` | `classify_request` (`route`) | 12 coding / 10 general | 8 | `summarise_failure` |
| `chain-agent-beam.json` | `classify_request` (`route`) | 16 coding / 10 general | 8 | `summarise_failure` |
| `chain-planner-default.json` | none | 16 | 6 | `end` (mission tools carry the report) |

The stages, and what each one is for:

**Entry classification.** The interactive chains open with a `route` task (`classify_request`) that sends the turn to a coding loop or a general loop — different system instructions, different budgets, same topology. The route labels are just the `equals` branch `when` values (`coding_change`, `general`), and even the router's failure mode is authored: its `on_failure` points at the general loop, so a broken classifier degrades to the safer default instead of killing the turn. `chain-agent-run.json` has no classifier — a one-shot pipeline invocation is its own classification.

**The main bounded loop.** Chat task, execute task, back-edge, budget — the anatomy above. The budget branch routes to recovery, not to `end`: spending the budget is treated as a signal the loop is stuck, not as a place to stop mid-thought.

**The recovery loop, with its own budget.** A second `chat_completion` task (`recovery_run`, `coding_recovery`, …) picks up the *same* chat history via `input_var`, under a fresh system instruction that says plainly: the main loop got stuck, continue from the actual history, do not restart. It gets its own execute task and its own smaller budget. The prompt even shows the model its live spend using the same counter the budget branch reads:

```text
BUDGET: You have already used {{edge_count:contenox_run->run_tools}} of 10 main and
{{edge_count:recovery_run->recovery_run_tools}} of 10 recovery tool_call rounds this turn.
```

**`summarise_failure`: report the true state.** When the recovery loop also fails or spends its budget, the chain ends in a task with `"tools": []` and `input_max_bytes: 8192` — it cannot act, and it cannot drown in the history that broke the run. Its instruction is the honesty contract: say what was attempted, what was actually done, where it got stuck, and what would unblock it. Do not pretend the work is complete.

**The bounds around every stage.** Each LLM task carries a `retry_policy` for transient provider errors — the shipped loops use `max_attempts: 4`, `initial_backoff: "1s"`, `max_backoff: "30s"`, `jitter: 0.25`, `rate_limit_min_wait: "10s"` — so a rate-limit blip does not kill a CI step. The chain-level `token_limit` (131072 in every shipped agent chain) bounds the context, with `"shift": true` sliding the window instead of erroring. And `tools_policies` pins the command surface per task: the shipped `local_shell` policy is an explicit `_allowed_commands` list (`ls,cat,echo,pwd,which,find,grep,git,go,…`) plus a `_denied_commands` list (`sudo,su,dd,mkfs,fdisk,parted,shred`), with matching byte and depth caps on `local_fs` and `webtools`. The [chains development doc](/docs/development/chains/) frames why: the chain is the reviewed execution contract around the loop.

**Where HITL sits.** The loop decides *what the model may attempt*; the active [HITL policy](/docs/guide/hitl/) decides *what actually runs unattended*. An `approve` rule pauses a tool call mid-loop — the `execute_tool_calls` task waits, a human answers, the loop resumes. That envelope is a separate authored file, so the same chain can run gated in one workspace and free in another.

The planner variant, `chain-planner-default.json`, proves the topology is the constant and everything else is configuration: same chat → tools → back-edge skeleton, same budget branches, but its `tools` array grants only `"mission"` — it plans, it never executes — and its recovery routes to `end` because the mission tools themselves are the reporting channel.

## Creating one: copy, then delete

The doctrine, stated as doctrine: **do not invent a loop topology. Copy the leanest shipped `chain-agent-*.json` and strip or extend it.** The shipped chains already encode the answers to the questions a hand-rolled loop gets wrong — where the budget branch goes, what `on_failure` catches, why the terminal task has no tools. The leanest general template is `chain-agent-run.json`: five tasks, no classifier, one persona.

Walk one adaptation end-to-end — a narrow diff-review loop, single tool, attended use:

```bash
cp ~/.contenox/system/chain-agent-run.json ./chain-agent-diffreview.json
```

1. **Delete the stages you don't need.** Remove `recovery_run`, `recovery_run_tools`, and `summarise_failure` — three tasks gone.
2. **Remove `"on_failure": "recovery_run"`** from the main task. An error now terminates the run, which is acceptable when a human is watching the output.
3. **Repoint the budget branch** from `recovery_run` to `end`, and lower `"when"` from `"10"` to `"6"` — a review should not need ten tool rounds.
4. **Narrow the tools.** `"tools": ["*"]` becomes `["local_shell"]` on *both* tasks, `_allowed_commands` shrinks to `git,go`, and the now-unused `local_fs` / `webtools` policy blocks go away. Keep the chat task's grant and the execute task's grant identical — the shipped chains repeat the same `tools` and `tools_policies` on both, and that symmetry is what makes the contract reviewable in one read.
5. **Rename the tasks** (`contenox_run` → `review`, `run_tools` → `review_tools`) and rewrite `system_instruction` to the reviewer persona. Drop `retry_policy` and `max_tokens` — this is the minimal set.

> **Note:** the `edge` string in `edge_traversed_at_least` spells the literal task ids: `"review->review_tools"`. Renaming a task means renaming every edge that mentions it — `contenox vet` catches the dangling reference if you miss one.

The result is the minimal agentic loop — derived from the real template, complete and valid:

```json
{
  "id": "chain-agent-diffreview",
  "description": "Bounded single-tool review loop derived from chain-agent-run.json: read the piped diff, run read-only git commands, answer once.",
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

`contenox vet ./chain-agent-diffreview.json` must pass it — the linter checks handler signatures, dataflow across every `goto` and `on_failure` edge, and branches that can never fire. Then pipe work into it:

```bash
git diff | contenox run --chain ./chain-agent-diffreview.json "review this diff"
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
