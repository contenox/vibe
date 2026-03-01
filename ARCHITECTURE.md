# Architecture Overview

Contenox provides a single, unified "Task Engine" that processes deterministic workflows (explicit state machines, branching, multi-model execution, external action hooks).

This core task engine `github.com/contenox/vibe/taskengine` is identical across our two main products, but the infrastructure providing persistence and messaging changes depending on the environment.

## The Two Environments

### 1. `vibe` (Local CLI)
**Goal:** Operate locally with minimal friction, perfect for single-developer workflows or admin scripting.
- **Entrypoint:** `cmd/vibe`
- **Database:** SQLite (single file: `.contenox/local.db`, powered by `libdbexec`)
- **Messaging:** In-memory bus (`libbus/inmem.go`)
- **Tokenizer:** Estimate tokenizer (no external service required)
- **Use Case:** Executing workflows directly via terminal, with optional access to the host machine via the `local_shell` and `local_fs` hooks.


### 2. `runtime-api` (Server)
**Goal:** Enterprise scale, observability, and concurrent execution.
- **Entrypoint:** `cmd/runtime-api`
- **Database:** PostgreSQL (with `JSONB` support for complex execution state)
- **Messaging:** NATS (for event bridging, pub/sub, canceling distributed jobs)
- **Tokenizer:** Remote gRPC/HTTP Tokenizer Service (exact token counts)
- **Use Case:** Headless workflow execution triggered by API gateways, Webhooks, or event bridges. Deployed via Docker/Kubernetes.

## The Core Design Philosophy

1. **Shared Interfaces (`libdbexec`, `libbus`, `Tokenizer`)**:
   - The core engine logic (`runtimestate`, `taskengine`, `llmrepo`) does not know whether it runs locally or on a server. It accepts interfaces.
   - We avoid `if local { ... } else { ... }` in shared code. The divergence happens purely in the wiring inside the `cmd/{entrypoint}/main.go` bootstrap phases.

2. **Explicit State Machines over Opaque Prompt Chains**:
   - Instead of a single massive LangChain-style loop, Contenox executes nodes (`Task`s) explicitly.
   - States handle condition branching, JS execution via Goja, embedding generation, or passing control to remote API hooks seamlessly.

3. **Hooks (Extensibility)**:
   - External services are registered via OpenAPI v3 specs (`/openapi.json`). Contenox automatically creates callable tools for LLMs matching those remote endpoints, injecting required authentication headers server-side to hide credentials from LLMs.

## Directory Structure Highlights
- `taskengine/` - Shared core: state machine transitions, handler execution.
- `internal/vibecli/` - Specific CLI logic, execution planner, user interaction loops.
- `localhooks/` - Handlers uniquely scoped to running code/commands on a host operating system.
- `libdbexec/` - Interfaces and adapters for PostgreSQL and SQLite.
- `docs/` - Auto-generated API specs and user guides.
