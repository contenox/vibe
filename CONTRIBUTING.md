# Contributing to Contenox

Thank you for your interest in contributing to Contenox! Whether you want to fix a bug, improve documentation, or propose a new feature, your help is welcome.

## Code of Conduct

Please treat all contributors with respect. Engage in constructive discussions and assume good intentions.

## Repository Structure

The **`contenox`** binary is the main entrypoint: plans, chat, and other CLI commands, plus **`contenox beam`**, which runs the HTTP server that **serves the Beam web UI** (embedded in the binary) and backs it with the same engine.

## Local Development Setup

### Prerequisites
- [Go](https://go.dev/doc/install) 1.24+
- Access to an LLM provider (e.g. OpenAI API key, or locally via [Ollama](https://ollama.ai/), [vLLM](https://docs.vllm.ai/), etc.)
- Docker & Docker Compose (only if you are working on containerized or multi-service deployments)
- `make`

### Building the CLI
```bash
# Build the binary into ./bin/contenox
make build-contenox

# Run an example
./bin/contenox "list files in my home directory"
```

### Running Beam (HTTP server + UI)
```bash
make build-contenox
./bin/contenox beam   # default :8081 — Beam UI; HTTP contract in docs/openapi.json (run make docs-gen after route changes)
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
5. **No Breaking Changes:** Avoid breaking existing workflows or changing the API schema unexpectedly. Keep the HTTP API backward compatible when you change routes or DTOs.

## Generating Documentation

The OpenAPI spec (`docs/openapi.json` and `docs/openapi.yaml`) is auto-generated from the Go types.
If you modify server endpoints or shapes, regenerate the documentation:
```bash
make docs-html
```
