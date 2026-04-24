# Contributing to Contenox

Thank you for your interest in contributing to Contenox! Whether you want to fix a bug, improve documentation, or propose a new feature, your help is welcome.

## Code of Conduct

Please treat all contributors with respect. Engage in constructive discussions and assume good intentions.


## Architecture

Contenox is a CLI-first Go application with clear layering:

```
CLI (runtime/contenoxcli/)
    ↓
Service Layer (runtime/*service/)
    ↓
Task Engine (runtime/taskengine/)
    ↓
Data + Integrations (lib*/ + runtime/runtimetypes/)
```

### Abstraction layers

**Service Layer** — each domain gets its own interface + implementation package (`planservice`, `execservice`, `backendservice`, `mcpserverservice`, `stateservice`, `hitlservice`, `terminalservice`, `vfsservice`, etc.). Services don't call each other directly; they communicate through the shared `runtimetypes.Store` interface and bus events.

**Task Engine** (`runtime/taskengine/`) — the core execution model. Chains are JSON DAGs with typed I/O (`DataType`: String, Int, JSON, ChatHistory). Task handlers (`prompt_to_string`, `chat_completion`, `execute_tool_calls`, `hook`, `noop`, etc.) are an enum. Branch conditions (`equals`, `contains`, `in_range`, `>`, `<`) are declarative — no Go code lives inside chain definitions.

**LLM Resolution** — two-level indirection: `llmrepo.ModelRepo` handles request-side selection (pick by capability or context length); `modelrepo.Provider` handles provider-side calls (Ollama, OpenAI, Gemini, Vertex, vLLM, local llama.cpp). `runtimestate` reconciles live backend capabilities every 10 s.

**Hook System** — chains invoke hooks by name; resolution happens at runtime. Hook types: `local_shell`, `filesystem`, `http`, `ssh`, `mcp`, `hitl`, `print`. MCP hooks route via a stdio session pool. Adding a new integration does not require touching the engine.

**Event-driven async** — `libbus` abstracts an in-memory SQLite-backed bus. Services publish typed events (e.g. `task.events.step_completed`) that other services subscribe to. Coupling between services is through events, not direct calls.

**Key files to orient yourself:**

| File | What it shows |
|------|---------------|
| `runtime/taskengine/tasktype.go` | Task types, handlers, branch operators |
| `runtime/planservice/planservice.go` | Plan orchestration interface |
| `runtime/internal/runtimestate/state.go` | Backend state sync |
| `runtime/contenoxcli/cli.go` | CLI dispatch |
| `runtime/contenoxcli/engine.go` | CLI-local engine bootstrap |


## Repository structure

The **`contenox`** binary is the main (only) entrypoint: `init`, `plan`, `chat`, `run`, `hook`, `mcp`, `backend`, `config`, `model`, `doctor`, `session`.

All AI/LLM orchestration packages live under **`runtime/`**. Infrastructure libraries (`libauth`, `libbus`, `libcipher`, `libdbexec`, `libkvstore`, `libroutine`, `libtracker`) stay at the module root.

```
runtime/              ← AI/LLM orchestration (taskengine, planservice, runtimetypes, …)
  internal/           ← model repo drivers, hooks, state reconciler
runtime/contenoxcli/  ← CLI command implementations
runtime/errdefs/      ← shared error sentinels
runtime/version/      ← embedded version string
cmd/contenox/         ← contenox binary entry point
lib*/                 ← infrastructure libraries (no LLM dependencies)
```

### Makefile overview

The root **`Makefile`** groups targets by prefix:

| Prefix | Purpose |
|--------|---------|
| **`build-*`** | `build-contenox` |
| **`test-*`** | Go tests, CLI help check |
| **`dev-*`** | `dev-install` / `dev-link` / `dev-unlink`, `dev-go-watch` (Air live reload) |
| **`deps-*`** | `deps-go-watch` (Air) |
| **`clean`** | Remove `bin/` |

Run **`make help`** at the repo root for the full list (default goal).

Version bumps and release notes for maintainers live in **`Makefile.version`** (`make -f Makefile.version help`).

## Local development setup

### Prerequisites

- [Go](https://go.dev/doc/install) 1.25+
- Access to an LLM provider (e.g. OpenAI API key, or locally via [Ollama](https://ollama.ai/), [vLLM](https://docs.vllm.ai/), etc.)
- `make`
- **Optional:** [Air](https://github.com/air-verse/air) for Go live reload (`make deps-go-watch`)

### Go binary path

`go install` puts binaries in `$GOPATH/bin` (typically `~/go/bin`). Add it to your shell:

```bash
export PATH=$PATH:$(go env GOPATH)/bin
```

Add this line to `~/.bashrc` or `~/.zshrc` to make it permanent.

### Building the CLI

```bash
# Build the binary into ./bin/contenox
make build-contenox

# Run an example
./bin/contenox "list files in my home directory"
```

Optional: **`make dev-install`** (or **`make dev-link`** after a build) symlinks `contenox` to `~/.local/bin/contenox` for development.

### Building with local LLM inference (CGo)

The `runtime/internal/modelrepo/local` package embeds llama.cpp inference directly into the binary via `github.com/ollama/ollama/llama` (CGo). This is **required** to build the full binary — `CGO_ENABLED=0` no longer works.

**System packages (Ubuntu/Debian):**

```bash
sudo apt-get install -y gcc g++ nlohmann-json3-dev
```

`nlohmann-json3-dev` provides `nlohmann/json_fwd.hpp`, which llama.cpp's v0.17.5 C++ tools require.

**Missing headers in the Go module cache:**

The ollama module at v0.17.5 includes multimodal C++ code (`mtmd`) that depends on two single-file libraries not bundled with the module. Download them manually:

```bash
OLLAMA_MOD="$(go env GOPATH)/pkg/mod/github.com/ollama/ollama@v0.17.5"
MTMD="$OLLAMA_MOD/llama/llama.cpp/tools/mtmd"

# Make module cache writable (read-only by default)
chmod -R u+w "$OLLAMA_MOD/llama"

# miniaudio — audio I/O library
mkdir -p "$MTMD/miniaudio"
curl -fsSL https://raw.githubusercontent.com/mackron/miniaudio/master/miniaudio.h \
     -o "$MTMD/miniaudio/miniaudio.h"

# stb_image — image loading library
mkdir -p "$MTMD/stb"
curl -fsSL https://raw.githubusercontent.com/nothings/stb/master/stb_image.h \
     -o "$MTMD/stb/stb_image.h"
```

These steps are one-time per machine. After that, `make build-contenox` (which sets `CGO_ENABLED=1`) will compile cleanly.

## Running tests

Before submitting a pull request, ensure tests pass.

**Fast path (matches CI):** tests whose names start with `TestUnit_`, with `-short` and a 15-minute cap:

```bash
make test-unit
```

**Full Go suite:** includes `TestSystem_*` integration tests (Docker, Ollama, vLLM containers, etc.) — slower and needs those tools:

```bash
make test              # GOMAXPROCS=4 go test ./...
make test-system       # only TestSystem_*
```

**CLI package (verbose):**

```bash
make test-contenox-verbose
```

**CLI help drift check** (after changing Cobra commands or flags): build first, then:

```bash
make test-contenox-help
```

**Race detector (optional):**

```bash
go test -race ./... -run '^TestUnit_'
```

## Pull request guidelines

1. **Create an issue:** If you're adding a major feature or changing the architecture, please open an issue first to discuss the design.
2. **Branch naming:** Create a branch from `main` with a descriptive name (`feature/xyz`, `fix/abc`, `docs/def`).
3. **Commit messages:** Follow [Conventional Commits](https://www.conventionalcommits.org/). For example, `feat: add support for local mode`, `fix: correct token count logic`, `docs: clarify CLI usage`.
4. **Style checks:** Ensure your code runs successfully through `gofmt` and standard linters like `golangci-lint` if available.

## Code conventions

### Go style

- **No comments.** Do not add doc comments, function-level comments, or inline comments. Well-named identifiers are self-documenting; the git log is the changelog.
- **Interfaces, not implementations.** Service constructors accept interfaces. Wire concrete types in `runtime/contenoxcli/engine.go`.
- **Declarative chains over Go code.** Business logic belongs in JSON task-chain definitions, not in new Go functions. Extend `taskengine` only when you need a new primitive that cannot be expressed as a chain.
- **Wide interfaces are a smell.** `runtimetypes.Store` is intentionally broad for historical reasons; new code should accept the narrowest interface slice it actually needs.
