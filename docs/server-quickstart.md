
## Get started with Runtime API

**Runtime API** is the HTTP server: Postgres, NATS, tokenizer, optional Ollama. Run it with Docker Compose for full-stack deployment. See [docs/runtime-api.md](docs/runtime-api.md) for running the binary locally, env vars, health, and API reference.

### Prerequisites

  * Docker and Docker Compose
  * `curl` and `jq`

### Run the Bootstrap Script

```bash
# Clone the repository
git clone https://github.com/contenox/vibe.git
cd vibe

# Configure the systems fallback models
export EMBED_MODEL=nomic-embed-text:latest
export EMBED_PROVIDER=ollama
export EMBED_MODEL_CONTEXT_LENGTH=2048
export TASK_MODEL=phi3:3.8b
export TASK_MODEL_CONTEXT_LENGTH=2048
export TASK_PROVIDER=ollama
export CHAT_MODEL=phi3:3.8b
export CHAT_MODEL_CONTEXT_LENGTH=2048
export CHAT_PROVIDER=ollama
export OLLAMA_BACKEND_URL="http://ollama:11434"
# or any other like: export OLLAMA_BACKEND_URL="http://host.docker.internal:11434"
# to use OLLAMA_BACKEND_URL with host.docker.internal
# remember sudo systemctl edit ollama.service -> Environment="OLLAMA_HOST=172.17.0.1" or 0.0.0.0

# Start the container services
echo "Starting services with 'docker compose up -d'..."
docker compose up -d
echo "Services are starting up."

# Configure the runtime with the preferenced models
# the bootstraping script works only for ollama models/backends
# for to use other providers refer to the API-Spec.
./scripts/bootstrap.sh $EMBED_MODEL $TASK_MODEL $CHAT_MODEL
# setup a demo OpenAI chat-completion and model endpoint
./scripts/openai-demo.sh $CHAT_MODEL demo
# this will setup the following endpoints:
# - http://localhost:8081/openai/demo/v1/chat/completions
# - http://localhost:8081/openai/demo/v1/models
#
# example:
# docker run -d -p 3000:8080 \
# -e OPENAI_API_BASE_URL='http://host.docker.internal:8081/openai/demo/v1' \
# -e OPENAI_API_KEY='any-key-for-demo-env' \
# --add-host=host.docker.internal:host-gateway \
# -v open-webui:/app/backend/data \
# --name open-webui \
# --restart always \
# ghcr.io/open-webui/open-webui:main
```

Once the script finishes, the environment is fully configured and ready to use.

-----

### Try It Out: Execute a Prompt

After the bootstrap is complete, test the setup by executing a simple prompt:

```bash
curl -X POST http://localhost:8081/execute \
  -H "Content-Type: application/json" \
  -d '{"prompt": "Explain quantum computing in simple terms"}'
```

### Next Steps: Create a Workflow

Save the following as `qa.json`:

```json
{
  "input": "What's the best way to optimize database queries?",
  "inputType": "string",
  "chain": {
    "id": "smart-query-assistant",
    "description": "Handles technical questions",
    "tasks": [
      {
        "id": "generate_response",
        "description": "Generate final answer",
        "handler": "raw_string",
        "systemInstruction": "You're a senior engineer. Provide concise, professional answers to technical questions.",
        "transition": {
          "branches": [
            { "operator": "default", "goto": "end" }
          ]
        }
      }
    ]
  }
}
```

Execute the workflow:

```bash
curl -X POST http://localhost:8081/tasks \
  -H "Content-Type: application/json" \
  -d @qa.json
```

All runtime activity is captured in structured logs:

```bash
docker logs contenox-runtime-api
```


# Runtime API

**Runtime API** is the HTTP server product: Postgres, NATS, tokenizer, optional Ollama. It runs the same task-chain engine as **Vibe**, exposed over HTTP for full-stack deployment. For convenience, a Docker Compose is provided.

---

## Run with Docker Compose (recommended)

From the repo root:

```bash
docker compose up -d
```

Compose starts the **runtime-api** service plus Postgres, NATS, tokenizer, and optional Ollama. The API is exposed on the host at **http://localhost:8081** (container listens on 8080). Use the [bootstrap script](../scripts/bootstrap.sh) to configure models and demo endpoints. See the README section **Get started with Runtime API** for the full flow (env vars, `bootstrap.sh`, Try It Out).

---

## Run the server binary locally

Build and run without Docker:

```bash
make build-runtime-api
make run-runtime-api
```

Or run the binary directly:

```bash
./bin/runtime-api
```

You must set the required environment variables (see below) and have Postgres, NATS, and the tokenizer service reachable. Typical use: run dependencies in Docker and point the binary at them (e.g. `DATABASE_URL=postgres://...@localhost:5432/...`, `NATS_URL=nats://...@localhost:4222`, `TOKENIZER_SERVICE_URL=http://localhost:50051`).

---

## Environment variables

Configuration is read from the environment (lowercase keys map to config fields).

| Variable | Required | Description |
|----------|----------|-------------|
| `DATABASE_URL` | Yes | Postgres connection string (e.g. `postgres://user:pass@host:5432/db?sslmode=disable`) |
| `PORT` | No | HTTP port (default `8080`) |
| `NATS_URL` | Yes | NATS server URL |
| `NATS_USER` | No | NATS username |
| `NATS_PASSWORD` | No | NATS password |
| `TOKENIZER_SERVICE_URL` | Yes | Tokenizer gRPC endpoint (e.g. `http://tokenizer:50051`) |
| `EMBED_MODEL`, `EMBED_PROVIDER`, `EMBED_MODEL_CONTEXT_LENGTH` | No | Embedding model config (used by bootstrap) |
| `TASK_MODEL`, `TASK_PROVIDER`, `TASK_MODEL_CONTEXT_LENGTH` | No | Task model config |
| `CHAT_MODEL`, `CHAT_PROVIDER`, `CHAT_MODEL_CONTEXT_LENGTH` | No | Chat model config |
| `TOKEN` | No | API key / token |
| `VECTOR_STORE_URL`, `UI_BASE_URL`, `ADDR` | No | Optional service URLs |

---

## Health and API base URL

- **Health**: `GET /health` — use for readiness/liveness (e.g. `curl http://localhost:8081/health` when using compose).
- **API base URL**: When run via Docker Compose, the API is at **http://localhost:8081**. Key paths include `/execute`, `/tasks`, and OpenAI-compatible routes under `/openai/...` after bootstrap.

---

Use these for endpoint details, request/response schemas, and authentication (e.g. `X-API-Key` when `TOKEN` is set).

