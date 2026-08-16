---
title: Transitions & Branching
description: How a chain decides which task runs next — operators, edges, and on_failure.
order: 2
---

# Transitions & Branching

Task chains are state machines. When a task finishes running its `handler`, the chain evaluates its `transition` rules to determine which task to execute next.

```json
"transition": {
  "branches": [
    { "operator": "equals", "when": "tool_call", "goto": "run_tools" },
    { "operator": "default", "when": "",         "goto": "end" }
  ]
}
```

## How transitions work

1. The current task returns a result string (the "eval").
2. The engine checks the `transition.branches` array from top to bottom.
3. It evaluates the `when` condition against the eval string using the `operator`.
4. The first branch that evaluates to `true` determines the next step (`goto`).

If the branch specifies `"goto": "end"` (or an empty `goto`), the chain terminates successfully.

A task with **no branches at all** is a leaf: the chain ends cleanly with that task's output, rather than erroring. So a terminal task can omit `transition.branches` instead of authoring an explicit `{ "operator": "default", "goto": "end" }` — though the explicit form is still the way to end *conditionally*.

## `on_failure`

A task ID to jump to when the current task raises an error — evaluated before any branch conditions. If `on_failure` is absent and the task errors, the chain terminates.

```json
"transition": {
  "on_failure": "error_handler",
  "branches": [
    { "operator": "default", "when": "", "goto": "next_step" }
  ]
}
```

## Operators

| Operator | How it matches | Example |
|----------|---------------|---------|
| `equals` | Exact string match | `"when": "tool_call"` matches `"tool_call"` |
| `contains` | Substring match | `"when": "fail"` matches `"api_failure"` |
| `starts_with` | Prefix match | `"when": "err"` matches `"error_timeout"` |
| `ends_with` | Suffix match | `"when": "_ok"` matches `"write_ok"` |
| `edge_traversed_at_least` | Fires once an edge has been traversed N times this run; reads engine state, not task output | `"edge": "chat->run_tools", "when": "20"` |
| `default` | Always matches | Used as the fallback at the end of the array |

## What do tasks return?

Each handler returns a fixed **control token** as its eval — these are not the
model's text. To branch on what the model actually said, use `route`.

- **`chat_completion`**: `"tool_call"` (model requested tools) or `"executed"` (replied with text, no tool calls).
- **`execute_tool_calls`**: `"tools_executed"` (ran the calls), `"no_calls_found"` (model produced no tool calls), or `"noop"` (empty history).
- **`tools`**: `"tools_executed"` — or, when `output_template` is set, the rendered template string.
- **`route`**: the chosen label — one of this task's declared `equals` branch `when` values. The engine normalizes the model's answer: it tries a **case-insensitive exact** match against a label, then a **case-insensitive substring** match, and only falls through to the `default` branch if neither matches. Input passes through unchanged.
- **`noop`**: passes the input through; eval is `"noop"`.
- **`raise_error`**: terminates the chain with an error — no branch is evaluated.

> **Note:**
> You **cannot** branch on `"failed"`. When a task fails (including `execute_tool_calls` / `tools`), the engine raises an error and immediately routes to `on_failure` (or terminates the chain if `on_failure` is absent) — this happens *before* any branch is evaluated, so a `{ "when": "failed" }` branch can never fire. Handle failure with `on_failure`, not a branch.

Place a `default` branch last as the fallback. For agentic loops, put an `edge_traversed_at_least` branch ahead of the loop branch to bound iterations.

## Reading edge counts from a prompt

The same counter that backs `edge_traversed_at_least` is exposed to `system_instruction` (and other template fields) as a macro:

```text
{{edge_count:from_task_id->to_task_id}}
```

It expands at every task step to the live count of how many times that edge has been traversed in the current chain run, starting at `0`. Resolves to `0` for edges that have never fired (typos won't break the prompt mid-turn).

This unlocks a **self-paced agent** pattern: instead of splitting a 20-round budget across a main agent + a recovery agent that hands off at round 10 with a frozen "10 of 20" warning, you have **one** chat task whose `system_instruction` shows the live count. The model sees the budget grow on every turn and self-paces accordingly. When the `edge_traversed_at_least` ceiling fires, the chain still routes to a tool-less terminal task for a clean wrap-up. See [Self-paced agent with dynamic budget](/docs/specification/examples#4-self-paced-agent-with-dynamic-budget) for the full example.
