# Contributing to Contenox

Thank you for your interest in contributing to Contenox! Whether you want to fix a bug, improve documentation, or propose a new feature, your help is welcome.

## Code of Conduct

Please treat all contributors with respect. Engage in constructive discussions and assume good intentions.

## Repository Structure

This repository contains:
1. **`vibe`** (CLI): A local workflow engine and LLM interface using SQLite and an in-memory bus.
2. **`runtime-api`** (Server): The full HTTP server API utilizing PostgreSQL and NATS.
3. **`enterprise/`** (Submodule): Optional, private enterprise code. If you don't have access, ignore it—the OSS runtime works perfectly without it.

See [ARCHITECTURE.md](ARCHITECTURE.md) for a deeper conceptual breakdown.

## Local Development Setup

### Prerequisites
- [Go](https://go.dev/doc/install) 1.24+
- Access to an LLM provider (e.g. OpenAI API key, or locally via [Ollama](https://ollama.ai/), [vLLM](https://docs.vllm.ai/), etc.)
- Docker & Docker Compose (if you are working on the `runtime-api` server)
- `make`

### Building the CLI (`vibe`)
```bash
# Build the binary into ./bin/vibe
make build-vibe

# Run an example
make run-vibe ARGS="-input 'list files in my home directory'"
```

### Building the Server (`runtime-api`)
```bash
# Start the supporting infrastructure (Postgres, NATS)
docker compose up -d

# Build and run the server
make build-runtime-api
make run-runtime-api
```

## Running Tests

Before submitting a Pull Request, ensure all tests pass.

```bash
# Run all unit tests
go test ./...

# Run tests with race detection
go test -race ./...
```

## Pull Request Guidelines

1. **Create an Issue:** If you're adding a major feature or changing the architecture, please open an issue first to discuss the design.
2. **Branch Naming:** Create a branch from `main` with a descriptive name (`feature/xyz`, `fix/abc`, `docs/def`).
3. **Commit Messages:** Follow [Conventional Commits](https://www.conventionalcommits.org/). For example, `feat: add support for local mode`, `fix: correct token count logic`, `docs: clarify CLI usage`.
4. **Style checks:** Ensure your code runs successfully through `gofmt` and standard linters like `golangci-lint` if available.
5. **No Breaking Changes:** Avoid breaking existing workflows or changing the API schema unexpectedly. Keep `runtime-api` backward compatible.

## Generating Documentation

The OpenAPI spec (`docs/openapi.json` and `docs/openapi.yaml`) is auto-generated from the Go types.
If you modify server endpoints or shapes, regenerate the documentation:
```bash
make docs-html
```
