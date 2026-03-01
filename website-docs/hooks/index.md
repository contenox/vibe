# What are Hooks?

Hooks are capabilities the AI model can call from within a chain. Instead of limiting the model to text generation, hooks let it take actions: read files, run scripts, query databases, or call external APIs.

![Hooks Architecture](/hooks_architecture.png)

## Hook Types

Contenox supports three types of hooks:

1. **`local_shell`** — Execute bash commands on the host machine.
2. **`local_fs`** — Read and write files to the local filesystem.
3. **Remote Hooks** — Any external HTTP service that exposes an OpenAPI v3 spec.

## How the model uses them

In your chain JSON, you specify which hooks are available to the task:

```json
"execute_config": {
  "model": "qwen2.5:7b",
  "provider": "ollama",
  "hooks": ["nws", "local_shell"]
}
```

Behind the scenes:
1. The engine fetches the tools from those hooks (reading the local definitions or the remote `/openapi.json`).
2. It translates them into OpenAI-compatible tool schemas.
3. It passes them to the model alongside your prompt.
4. If the model chooses to call a tool, your chain's `execute_tool_calls` task runs it and feeds the result back.

## Adding Custom Capabilities

The easiest way to add custom capabilities to your agent is to write a small HTTP service (e.g. in Python FastAPI or Express) that serves an OpenAPI spec, then register it as a **Remote Hook**. See [Remote Hooks](/hooks/remote) for a full example.
