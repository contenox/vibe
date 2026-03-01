# Transitions & Branching

Task chains are state machines. When a task finishes running its `handler`, the chain evaluates its `transition` rules to determine which task to execute next.

```json
"transition": {
  "branches": [
    { "operator": "equals", "when": "tool-call", "goto": "run_tools" },
    { "operator": "default", "when": "",         "goto": "end" }
  ]
}
```

## How transitions work

1. The current task returns a result string (the "eval").
2. The engine checks the `transition.branches` array from top to bottom.
3. It evaluates the `when` condition against the eval string using the `operator`.
4. The first branch that evaluates to `true` determines the next step (`goto`).

If the branch specifies `"goto": "end"`, the chain terminates successfully.

## Operators

| Operator | How it matches | Example |
|----------|---------------|---------|
| `equals` | Exact string match | `"when": "tool-call"` matches `"tool-call"` |
| `not_equals` | Inverse exact match | `"when": "error"` matches `"ok"` |
| `contains` | Substring match | `"when": "fail"` matches `"api_failure"` |
| `not_contains` | Inverse substring match | `"when": "ok"` matches `"error"` |
| `range` | Numeric range (min,max) | `"when": "200,299"` matches `"201"` |
| `default` | Always matches | Used as the fallback at the end of the array |

## What do tasks return?

Each handler returns a different eval string that you can branch on:

- **`chat_completion`**: Returns `"stop"`, `"tool-call"`, or `"length"`.
- **`execute_tool_calls`**: Returns `"ok"` or `"error"`.
- **`condition_key`**: Returns the exact keyword the model chose (e.g. `"valid"` or `"invalid"`).
- **`hook`**: Usually returns `"ok"` or `"failed"`.

## Branch Composition

You can extract data from a task's output and store it as a new variable during the transition. Use the `compose` object on a branch:

```json
{
  "operator": "default",
  "when": "",
  "goto": "next_step",
  "compose": {
    "with_var": "extracted_id",
    "strategy": "json_path:$.user.id"
  }
}
```

If this branch is taken, the engine runs the JSONPath `$.user.id` against the task's output, and stores the result in a chain variable named `extracted_id`. Later tasks can use <span v-pre>`{{.extracted_id}}`</span> in their templates.
