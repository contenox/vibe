---
title: Annotated Examples
description: Full working task chains with commentary, from tool use to error handling.
---

# Annotated Examples

These chains show the handlers and transition patterns from the [specification](/docs/specification/) applied end to end.

## 1. The Default Chain (Tool Use)

This is the chain used for **interactive chat** — both `contenox chat "hello"` and a bare `contenox "hello"` (a bare prompt is session-backed chat, injected as `chat`) resolve `chain-agent-contenox.json` when no `--chain` is given. It defines a loop between the model and the tools. Only `contenox run` uses `chain-agent-run.json` — see the [CLI reference](/docs/reference/contenox-cli). The example below is simplified for clarity; the `chain-agent-contenox.json` that ships with `contenox init` is a fuller ~10-task chain with a router, a coding loop, and a recovery loop.

```jsonc
{
  "id": "default-chain",
  "description": "Standard interactive chat loop supporting tool calls.",
  "token_limit": 8192,
  "tasks": [
    {
      "id": "chat",
      "handler": "chat_completion",
      "execute_config": {
        "model": "{{var:model}}",
        "provider": "{{var:provider}}",
        "tools": ["local_shell", "local_fs"]
      },
      "transition": {
        "branches": [
          // If the model decides a tool is needed, loop to run_tools
          { "operator": "equals", "when": "tool_call", "goto": "run_tools" },
          // Otherwise, end the chain and wait for next user input
          { "operator": "default", "when": "", "goto": "end" }
        ]
      }
    },
    {
      "id": "run_tools",
      "handler": "execute_tool_calls",
      // input_var ensures it reads the tool calls from the 'chat' task output
      "input_var": "chat",
      "transition": {
        "branches": [
          // Once tools finish, loop back to the chat task to feed results in
          { "operator": "default", "when": "", "goto": "chat" }
        ]
      }
    }
  ]
}
```

## 2. Remote Tools Example (NWS)

This chain replaces the local shell tools with a remote API tools (the US National Weather Service). Notice the custom `system_instruction` providing domain-specific guidance on how to use the NWS tools.

```json
{
  "id": "chain-nws",
  "description": "Query the US National Weather Service via natural language.",
  "token_limit": 32768,
  "tasks": [
    {
      "id": "nws_chat",
      "handler": "chat_completion",
      "system_instruction": "You are a weather assistant with access to the US National Weather Service API. Use the tools to answer weather questions. Summarise results concisely — do NOT dump raw JSON or lists of hundreds of items. For forecasts you may need two calls: first the 'point' tool with latitude and longitude, then 'gridpoint_forecast' with the returned grid reference. For alerts, use 'alerts_active_area' with the two-letter state code. Today is {{now:2006-01-02}}.",
      "execute_config": {
        "model": "{{var:model}}",
        "provider": "{{var:provider}}",
        "tools": ["nws"]
      },
      "transition": {
        "branches": [
          { "operator": "equals", "when": "tool_call", "goto": "run_tools" },
          { "operator": "default", "when": "", "goto": "end" }
        ]
      }
    },
    {
      "id": "run_tools",
      "handler": "execute_tool_calls",
      "input_var": "nws_chat",
      "transition": {
        "branches": [
          { "operator": "default", "when": "", "goto": "nws_chat" }
        ]
      }
    }
  ]
}
```

## 3. Retry and Fallback Model

This chain calls an external API via `webtools`. `retry_policy` retries up to three times with exponential backoff and swaps to a cheaper model after two consecutive failures.

```json
{
  "id": "resilient-chain",
  "description": "Chat loop with retry on transient errors and a fallback model.",
  "token_limit": 16384,
  "tasks": [
    {
      "id": "chat",
      "handler": "chat_completion",
      "execute_config": {
        "model": "gpt-4.1",
        "provider": "openai",
        "tools": ["webtools"],
        "retry_policy": {
          "max_attempts": 3,
          "initial_backoff": "1s",
          "max_backoff": "30s",
          "jitter": 0.2,
          "fallback_model_id": "gpt-4.1-nano",
          "fallback_after": 2
        }
      },
      "transition": {
        "branches": [
          { "operator": "equals", "when": "tool_call", "goto": "run_tools" },
          { "operator": "default", "when": "", "goto": "end" }
        ]
      }
    },
    {
      "id": "run_tools",
      "handler": "execute_tool_calls",
      "input_var": "chat",
      "transition": {
        "branches": [
          { "operator": "default", "when": "", "goto": "chat" }
        ]
      }
    }
  ]
}
```

## 4. Self-paced agent with dynamic budget

A single chat task with the `{{edge_count:from->to}}` macro lets the model see its own remaining tool-call budget on every turn, instead of splitting the loop into a main agent and a fixed-round "recovery" agent. The model reads the live counter and paces itself. When the hard ceiling fires, the chain routes to a tool-less terminal task that produces a summary of what was attempted and where it got stuck.

```jsonc
{
  "id": "self-paced-chain",
  "description": "Single agent that sees its own remaining budget on every turn.",
  "token_limit": 131072,
  "tasks": [
    {
      "id": "chat",
      "handler": "chat_completion",
      // The {{edge_count:...}} expands per step to the live traversal count of
      // the named edge. The model reads the budget growing on every turn and
      // paces itself instead of guessing.
      "system_instruction": "You are a coding assistant.\n\nBUDGET: You have used {{edge_count:chat->run_tools}} of 20 tool-call rounds on this turn. Pace yourself — once the budget is spent you will be forced into a tool-less summary and cannot run any more tools. Prefer producing an answer once you have enough information; address root causes rather than retrying the same thing.",
      "execute_config": {
        "model": "{{var:model}}",
        "provider": "{{var:provider}}",
        "tools": ["*"]
      },
      "transition": {
        "on_failure": "summarise_failure",
        "branches": [
          // Hard ceiling: 20 rounds in, force a tool-less wrap-up.
          { "operator": "edge_traversed_at_least", "edge": "chat->run_tools", "when": "20", "goto": "summarise_failure" },
          // Otherwise loop while the model still wants tools.
          { "operator": "equals", "when": "tool_call", "goto": "run_tools" },
          // Model finished its reply — end the turn.
          { "operator": "default", "when": "", "goto": "end" }
        ]
      }
    },
    {
      "id": "run_tools",
      "handler": "execute_tool_calls",
      "input_var": "chat",
      "transition": {
        "branches": [
          { "operator": "default", "when": "", "goto": "chat" }
        ]
      }
    },
    {
      "id": "summarise_failure",
      "handler": "chat_completion",
      "input_var": "chat",
      // No tools — the model is forced to wrap up rather than start a new sub-quest.
      "system_instruction": "You've exhausted the tool-call budget for this turn. Tell the user what was attempted, what concrete steps were taken, where things got stuck, and what would unblock the work. Do not pretend the work is complete.",
      "execute_config": {
        "model": "{{var:model}}",
        "provider": "{{var:provider}}",
        "tools": [],
        "max_tokens": 16384
      },
      "transition": {
        "branches": [
          { "operator": "default", "when": "", "goto": "end" }
        ]
      }
    }
  ]
}
```

**Why this works:**
- The macro is re-evaluated at every task step, so each call to the `chat` task gets a fresh budget reading. There is no stale "10 of 20" line cached anywhere.
- The state machine is still authoritative on the ceiling — `edge_traversed_at_least` is the hard stop. The macro just lets the model see the same number the engine is enforcing.
- If a budget transition fires while the model has an unanswered tool call hanging from the last LLM turn, the engine executes it inline before the next step rather than handing the provider a dangling `tool_call`. No half-pruned histories, no `MALFORMED_FUNCTION_CALL`.

## 5. Error Handling with `on_failure`

`on_failure` routes to a named task whenever the current task raises an uncaught error — before any branch conditions are checked. Here both `edit` and `run_tools` point to `bail`, which calls `raise_error` to terminate cleanly instead of leaving the run in an undefined state.

```json
{
  "id": "safe-chain",
  "description": "File-editing chain with explicit error handling.",
  "token_limit": 8192,
  "tasks": [
    {
      "id": "edit",
      "handler": "chat_completion",
      "execute_config": {
        "model": "{{var:model}}",
        "provider": "{{var:provider}}",
        "tools": ["local_fs"]
      },
      "transition": {
        "on_failure": "bail",
        "branches": [
          { "operator": "equals", "when": "tool_call", "goto": "run_tools" },
          { "operator": "default", "when": "", "goto": "end" }
        ]
      }
    },
    {
      "id": "run_tools",
      "handler": "execute_tool_calls",
      "input_var": "edit",
      "transition": {
        "on_failure": "bail",
        "branches": [
          { "operator": "default", "when": "", "goto": "edit" }
        ]
      }
    },
    {
      "id": "bail",
      "handler": "raise_error"
    }
  ]
}
```

