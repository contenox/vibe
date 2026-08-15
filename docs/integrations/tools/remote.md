---
description: Point at any HTTP service's OpenAPI spec and every operation becomes a callable, allowlistable tool — no client code required.
---

# Remote Tools

Remote tools turn any external HTTP service into a set of callable tools for your AI agent.

> An agent can also bring a service with it, rather than you registering one for the whole machine: `remoteTools:` in an [agent declaration](/docs/guide/agents/#tools-an-agent-brings-with-it) registers the same thing scoped to that agent, and retires it when the declaration is deleted. Contenox fetches the service's OpenAPI v3 spec, discovers every operation, and makes them available to the model as named tools — no client code required.

By default Contenox looks for the spec at `<url>/openapi.json`. Use `--spec` to point at a different location — another URL, a local file, or a hand-crafted subset spec — without changing where API calls are sent.

## Register a tools

```bash
contenox tools add <name> --url <endpoint>

# Example: US National Weather Service (free, public API)
contenox tools add nws --url https://api.weather.gov --timeout 15000

# Example: internal API with auth and hidden tenant context
contenox tools add myapi --url https://api.example.com \
  --header "Authorization: Bearer $MY_TOKEN" \
  --inject "tenant_id=acme" \
  --inject "env=production"

# Example: legacy API where the spec lives at a different path
contenox tools add erpnext --url https://erp.example.com \
  --spec ~/.contenox/erp-subset.yaml \
  --header "Authorization: token $ERP_TOKEN"

# Example: spec hosted on a raw GitHub URL
contenox tools add legacy --url https://api.example.com \
  --spec https://raw.githubusercontent.com/example/repo/main/openapi.yaml

# Example: session-cookie login (Frappe / ERPNext)
contenox tools add erp --url https://erp.local \
  --insecure-skip-tls-verify \
  --auth-login-url https://erp.local/api/method/login \
  --auth-login-body '{"usr":"${FRAPPE_USER}","pwd":"${FRAPPE_PASS}"}' \
  --auth-extract-cookie sid \
  --auth-inject-header Cookie

# Example: JSON token login (custom auth endpoint)
contenox tools add myapi --url https://api.example.com \
  --auth-login-url https://api.example.com/auth/token \
  --auth-login-body '{"username":"${API_USER}","password":"${API_PASS}"}' \
  --auth-extract-jsonpath '$.data.token' \
  --auth-inject-header Authorization \
  --auth-inject-format "Bearer %s"
```

Contenox probes the endpoint at registration time to count available tools. If the service is unreachable at that moment, it is still registered and re-probed at chain execution time.

### Flags

| Flag | Description |
|---|---|
| `--url` | Base URL of the service — where API calls are sent (required) |
| `--spec` | URL or local file path of the OpenAPI v3 spec. Accepts `https://...`, `~/path`, `./path`, `/abs/path`. Local paths are stored as `file://` URIs and must exist at registration time. Defaults to `<url>/openapi.json` when not set. |
| `--header` | HTTP header to inject on every call, e.g. `"Authorization: Bearer $TOKEN"` (repeatable) |
| `--inject` | Tool call argument to inject and hide from the model, e.g. `"tenant_id=acme"` (repeatable) |
| `--timeout` | Request timeout in milliseconds (default: 10000) |
| `--insecure-skip-tls-verify` | Skip TLS certificate verification. Use only for internal/self-signed services. |
| `--auth-login-url` | URL to POST credentials to before calling the API. Enables the automatic http_handshake auth flow. |
| `--auth-login-method` | HTTP method for the login request (default: `POST`) |
| `--auth-login-body` | JSON body for the login request. Env vars are expanded at execution time, e.g. `'{"usr":"${USER}","pwd":"${PASS}"}'` |
| `--auth-extract-cookie` | Name of the `Set-Cookie` cookie to extract from the login response |
| `--auth-extract-jsonpath` | JSONPath expression to extract a token from the login response body, e.g. `$.data.token` |
| `--auth-inject-header` | HTTP header to carry the extracted token on subsequent API calls, e.g. `Cookie` or `Authorization` |
| `--auth-inject-format` | Printf format for the injected value, e.g. `"Bearer %s"`. Defaults to `"name=value"` when extracting a cookie. |

## Inspect tools

```bash
contenox tools show nws
```

Lists the tools's URL, timeout, registered headers (keys only — values are never shown), injected params (keys only — values hidden), and all tools discovered from its OpenAPI spec.

## Manage tools

```bash
contenox tools list                               # show all registered tools
contenox tools update nws --timeout 30000         # update timeout
contenox tools update nws --header "X-App: v2"    # replace ALL headers
contenox tools update nws --inject "tenant_id=newvalue"  # replace ALL inject params
contenox tools update nws --spec ~/specs/nws-v2.yaml     # replace spec source
contenox tools update nws --spec ""              # clear spec (revert to <url>/openapi.json)
contenox tools remove nws
```

> **Important:**
> `tools update --header` **replaces** the entire header set for the tools. `tools update --inject` **replaces** the entire inject param map. `tools update --spec ""` clears the spec override and reverts to `<url>/openapi.json` discovery. Pass all required values in a single update call.

## Authentication and secret injection

Pass authentication headers at registration time:

```bash
contenox tools add myapi --url https://api.example.com \
  --header "Authorization: Bearer $MY_TOKEN" \
  --header "X-Tenant: acme"
```

These headers are stored in SQLite and injected transparently into every HTTP call made to that service. **The model never sees them** — they are stripped from the tool schema before it reaches the LLM.

## Injecting tool call arguments (hidden from model)

Beyond HTTP headers, you can also inject named parameters directly into every tool call — completely hidden from the model's tool schema:

```bash
contenox tools add myapi --url https://api.example.com \
  --inject "tenant_id=acme" \
  --inject "correlation_id=trace-123"
```

Specifically, the engine:
1. Removes injected parameter names from the tool manifest the model sees (`properties` + `required`)
2. Merges them back into every tool call **after** the model-provided args (injected values always win)

This is the right pattern for: tenant IDs, correlation/trace IDs, session context, environment tags, and any other infrastructure concern that the model shouldn't reason about.

## Auto-login (http_handshake auth flow)

Some APIs — ERPNext/Frappe, custom enterprise services, legacy internal tools — use session-based authentication: you POST credentials to a login endpoint, receive a session cookie or JSON token, and then carry that token on every subsequent call.

Contenox handles this automatically with the `http_handshake` auth flow. When a tool call receives a `401` or `403`, the runtime:

1. **POSTs credentials** to `--auth-login-url` with the provided body (env vars in the body are expanded at execution time).
2. **Extracts a token** from the response — either a named `Set-Cookie` cookie (`--auth-extract-cookie`) or a value from the JSON body via JSONPath (`--auth-extract-jsonpath`).
3. **Injects the token** as an HTTP header on all subsequent calls (`--auth-inject-header`), optionally formatted via `--auth-inject-format`.
4. **Retries** the original tool call with the new token.
5. **Persists** the new token back to the database so future invocations start authenticated without re-running the login.

### Pattern A: session cookie (e.g. Frappe / ERPNext)

```bash
contenox tools add erp --url https://erp.local \
  --insecure-skip-tls-verify \
  --auth-login-url https://erp.local/api/method/login \
  --auth-login-body '{"usr":"${FRAPPE_USER}","pwd":"${FRAPPE_PASS}"}' \
  --auth-extract-cookie sid \
  --auth-inject-header Cookie
```

`--auth-extract-cookie sid` picks the `sid` cookie from `Set-Cookie`. Because no `--auth-inject-format` is given, the header value is automatically formatted as `sid=<value>` — the correct HTTP `Cookie:` syntax.

### Pattern B: JSON token (Bearer / custom)

```bash
contenox tools add myapi --url https://api.example.com \
  --auth-login-url https://api.example.com/auth/token \
  --auth-login-body '{"username":"${API_USER}","password":"${API_PASS}"}' \
  --auth-extract-jsonpath '$.data.token' \
  --auth-inject-header Authorization \
  --auth-inject-format "Bearer %s"
```

`--auth-extract-jsonpath '$.data.token'` reads the token from the login response body. `--auth-inject-format "Bearer %s"` formats it into the `Authorization` header.

### Self-signed / internal TLS

```bash
contenox tools add intranet --url https://192.168.1.50 \
  --insecure-skip-tls-verify
```

> **Caution:**
> `--insecure-skip-tls-verify` disables TLS certificate verification entirely. Only use this for known internal services with self-signed certificates. Never use it against public endpoints.



## Tool naming

Contenox derives a tool name for each API operation in this priority order:

1. **`operationId`** from the OpenAPI spec (recommended)
2. **`x-tool-name`** extension on the operation
3. **Fallback**: `<last_path_segment>_<method>` (e.g. `alerts_get`)

For the best experience, set `operationId` on every operation in your OpenAPI spec.

## Excluded paths

The following paths are automatically excluded from tool discovery:

- `/health`, `/healthz` — health checks
- `/ready`, `/readyz` — readiness probes
- `/metrics` — Prometheus metrics

## Use in a chain

Add the tools's name to `execute_config.tools`:

```json
{
  "id": "weather_task",
  "handler": "chat_completion",
  "system_instruction": "You are a weather assistant. Available tools: {{toolservice:list}}.",
  "execute_config": {
    "model": "qwen3:8b",
    "provider": "ollama",
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
  "input_var": "weather_task",
  "transition": {
    "branches": [
      { "operator": "default", "when": "", "goto": "weather_task" }
    ]
  }
}
```

### Deterministic call: the `tools` task handler

A `tools` task invokes one specific tool of one provider directly, with no
model in the loop. `name` is the registered provider; `tool_name` is the
discovered operation on it. `output_template` is an optional Go template that
reshapes the tool's JSON response before transitions are evaluated, so
branch operators route on the extracted value rather than the raw body:

```json
{
  "id": "active_alert_count",
  "handler": "tools",
  "tools": {
    "name": "nws",
    "tool_name": "alerts_active_count"
  },
  "output_template": "{{.total}}",
  "transition": {
    "branches": [
      { "operator": "equals", "when": "0", "goto": "all_clear" },
      { "operator": "default", "when": "", "goto": "report_alerts" }
    ]
  }
}
```

## Building your own tools with FastAPI

FastAPI serves an `/openapi.json` spec automatically, making it a perfect fit. Every endpoint becomes a tool the moment you register the service.

```python
from fastapi import FastAPI

app = FastAPI()

@app.get("/summarize", operation_id="summarize_text")
def summarize(text: str) -> dict:
    """Return a short summary of the provided text."""
    return {"summary": text[:100] + "..."}
```

```bash
# Start the service
uvicorn main:app --port 8080

# Register it — spec is auto-discovered at http://localhost:8080/openapi.json
contenox tools add myapp --url http://localhost:8080
contenox tools show myapp   # → 1 tool: summarize_text
```

For services that don't serve their spec at `/openapi.json` (legacy APIs, ERPNext, custom paths), use `--spec`:

```bash
# Hand-crafted spec in your home directory
contenox tools add legacy --url https://erp.example.com \
  --spec ~/.contenox/erp-subset.yaml

# Spec at a custom HTTP path
contenox tools add mylegacy --url https://api.example.com \
  --spec https://api.example.com/v2/swagger.json
```

The model can now call `summarize_text` directly from any chain that includes `myapp` in its `tools` list.
