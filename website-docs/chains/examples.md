# Annotated Examples

Learning by example is the fastest way to understand task chains.

## 1. The Default Chain (Tool Use)

This is the chain that runs when you run `vibe "hello"` without explicitly providing a `--chain` flag. It defines a loop between the model and the tools.

```json
{
  "id": "default-chain",
  "description": "Standard interactive chat loop supporting tool calls.",
  "token_limit": 8192,
  "tasks": [
    {
      "id": "chat",
      "handler": "chat_completion",
      "execute_config": {
        "model": "<span v-pre>{{var:model}}</span>",
        "provider": "<span v-pre>{{var:provider}}</span>",
        "hooks": ["local_shell", "local_fs"]
      },
      "transition": {
        "branches": [
          // If the model decides a tool is needed, loop to run_tools
          { "operator": "equals", "when": "tool-call", "goto": "run_tools" },
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

## 2. Remote Hook Example (NWS)

This chain replaces the local shell hooks with a remote API hook (the US National Weather Service). Notice the custom `system_instruction` providing domain-specific guidance on how to use the NWS tools.

```json
{
  "id": "chain-nws",
  "description": "Query the US National Weather Service via natural language.",
  "token_limit": 32768,
  "tasks": [
    {
      "id": "nws_chat",
      "handler": "chat_completion",
      "system_instruction": "You are a weather assistant with access to the US National Weather Service API. Use the tools to answer weather questions. Summarise results concisely — do NOT dump raw JSON or lists of hundreds of items. For forecasts you may need two calls: first the 'point' tool with latitude and longitude, then 'gridpoint_forecast' with the returned grid reference. For alerts, use 'alerts_active_area' with the two-letter state code. Today is <span v-pre>{{now:2006-01-02}}</span>.",
      "execute_config": {
        "model": "<span v-pre>{{var:model}}</span>",
        "provider": "<span v-pre>{{var:provider}}</span>",
        "hooks": ["nws"]
      },
      "transition": {
        "branches": [
          { "operator": "equals", "when": "tool-call", "goto": "run_tools" },
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
