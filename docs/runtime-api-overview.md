# Runtime API — Capabilities Overview

The **Runtime API** is the full-stack HTTP server product built on the same core task engine as `vibe`. It is deployed via Docker Compose and exposes a REST API over HTTP (default port 8081).

> Full machine-readable spec: [openapi.json](openapi.json) | [openapi.html](openapi.html) (interactive UI)
> Quickstart: [server-quickstart.md](server-quickstart.md)

---

## What it can do

### LLM Execution
| Path | What it does |
|------|-------------|
| `POST /execute` | Run a prompt through the default configured LLM |
| `POST /tasks` | Execute any task chain dynamically — the primary workflow entrypoint |
| `POST /chat` | Chat through a configured task chain (multi-turn) |
| `POST /openai/*/v1/chat/completions` | **OpenAI-compatible drop-in endpoint** — plug in any OpenAI client |
| `POST /openai/*/v1/models` | OpenAI-compatible model listing |
| `POST /embed` | Generate vector embeddings via the configured model |

### Infrastructure Management
| Domain | What you can do |
|--------|----------------|
| **Backends** | CRUD for LLM provider connections (Ollama, OpenAI, vLLM, Gemini). Get live runtime status via SSE. |
| **Models** | Declare models, trigger Ollama pulls, monitor download queue via SSE. |
| **Affinity Groups** | Organize backends and models into groups for routing (e.g. `embed`, `chat`, `task` tiers). |

### Workflow Definitions
| Domain | What you can do |
|--------|----------------|
| **Task Chains** | CRUD for JSON chain definitions — the state machine workflows the engine executes. |
| **Hooks (Remote)** | Register any external OpenAPI v3 endpoint as a callable LLM tool. Credentials injected server-side. |
| **Hooks (Local)** | Inspect locally available hooks and their JSON schemas. |

### Event System
The runtime includes a full event-sourcing and serverless function layer for webhook-driven automation:

| Domain | What you can do |
|--------|----------------|
| **Events** | Append, query, and stream events by type/source/aggregate/time range. Real-time SSE stream available. |
| **Event Bridge** | Define mappings that transform raw webhook payloads into typed domain events via `POST /ingest`. |
| **Event Triggers** | Bind domain event types to JS function executions. |
| **Functions** | CRUD for sandboxed JavaScript functions (Goja VM). Triggered by events or called directly from task chains. |

### Auth
All endpoints require an `X-API-Key` header when `TOKEN` is set in the server environment.

---

## The OpenAI drop-in

After running the bootstrap script, the runtime exposes an OpenAI-compatible API:

```bash
# Any OpenAI client works by pointing at:
OPENAI_API_BASE_URL=http://localhost:8081/openai/<demo>/v1
OPENAI_API_KEY=any-key   # accepted but not validated in demo mode
```

This means you can use Open WebUI, LiteLLM, LangChain, or any OpenAI SDK against the Runtime API with zero code changes.

---

## What makes it different from `vibe`

| | `vibe` (CLI) | Runtime API (Server) |
|---|---|---|
| Storage | SQLite (single file) | PostgreSQL |
| Messaging | In-memory | NATS |
| Deployment | Binary, no infra | Docker Compose / Kubernetes |
| Users | Single developer | Teams, multi-tenant |
| Event system | No | Yes (webhook ingestion, SSE) |
| JS Functions | No | Yes (sandboxed Goja VM) |
| OpenAI compat | No | Yes |
