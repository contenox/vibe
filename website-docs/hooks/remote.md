# Remote Hooks

Remote hooks turn any external HTTP API into a set of callable tools for your AI agent.

Contenox reads the service's `/openapi.json` file, discovers every API operation (GET, POST, etc.), extracts the parameters, and exposes them as native tool calls to the LLM.

## Managing Hooks

Use the `vibe hook` command to manage remote hooks. They are stored in `.contenox/local.db`.

### 1. Register a hook
```bash
vibe hook add <name> --url <endpoint_url>
```
Example using the US National Weather Service (free, public):
```bash
vibe hook add nws --url https://api.weather.gov --timeout 15000
```
*Note: The engine automatically appends `/openapi.json` to the URL you provide to fetch the spec.*

### 2. Inspect discovered tools
```bash
vibe hook show nws
```
This lists all tools discovered from the OpenAPI spec, along with their expected parameters. The NWS api exposes ~60 tools like `alerts_active_area` and `gridpoint_forecast`.

### 3. List and Remove
```bash
vibe hook list
vibe hook remove nws
```

## Authentication

If your API requires a token, you can pass headers when adding the hook:
```bash
vibe hook add github --url https://api.github.com --header "Authorization: Bearer my-token"
```
Headers are saved securely in the database and are never echoed back in `hook show` outputs. They are injected transparently into every tool call made to that service.

## Using a Remote Hook in a Chain

Simply add the hook's name to the `hooks` array in your task's `execute_config`:

```json
"execute_config": {
  "model": "qwen2.5:7b",
  "provider": "ollama",
  "hooks": ["nws"]
}
```

If the model decides to call `nws.alerts_active_area`, the `execute_tool_calls` handler will automatically make the correct HTTP GET request to `https://api.weather.gov/alerts/active/area` with the parameters the model chose, and feed the JSON response back into the chat.
