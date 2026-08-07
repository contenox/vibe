# Contributing to Contenox

Thanks for helping improve Contenox. This repository centers on the V1 runtime
surface: the `contenox` CLI, ACP over stdio for editors (Zed, JetBrains,
AionUi, OpenClaw), and the upcoming `contenox new` terminal UI. Why the
surface looks like this: see WHY.md.

## Code of Conduct

Treat contributors with respect. Keep technical disagreements concrete and
actionable.

## Architecture

Contenox is a single-binary agent with a thin set of host adapters:

```text
CLI / ACP stdio sessions / terminal UI
    ->
Service Layer (runtime/*service/)
    ->
Task Engine (internal/kernel/taskengine/)
    ->
Data + Integrations (lib*/ + internal/store/runtimetypes/ + internal/services/localtools/)
```

The Go runtime owns chains, execution state, model routing, tools, MCP worker
sessions, human-in-the-loop policy, session history, and local state. Editor
integrations are adapters around that runtime — they must not re-create chain
semantics elsewhere.

### V1 product surface

- `contenox` CLI (chat, run, sessions, config, backends, models, tools, MCP,
  workspace grants, sandbox inspection)
- `contenox acp` / `contenox acpx` for ACP editors
- `contenox new` — the terminal UI (in development, built on the same ACP
  session services)

When you change this surface, update the relevant user docs in the same
change.

### Abstraction layers

**Service Layer** — each domain gets its own interface and implementation
package (`execservice`, `backendservice`, `mcpserverservice`, `stateservice`,
`hitlservice`, `localfileservice`, `fleetservice`, `missionservice`, etc.).
Services communicate through the shared `runtimetypes.Store` interface and bus
events rather than depending on each other directly.

**Task Engine** (`internal/kernel/taskengine/`) — the core execution model. Chains are
JSON/YAML DAGs with typed I/O (`DataType`: String, Int, JSON, ChatHistory,
Any). Task handlers (`chat_completion`, `execute_tool_calls`, `tools`,
`route`, `raise_error`, `noop`) and branch operators (`equals`, `contains`,
`starts_with`, `ends_with`, `default`, `edge_traversed_at_least`) are
declarative. New Go primitives should be rare.

**LLM Resolution** — `llmrepo.ModelRepo` handles request-side selection by
capability, provider, model, and context length. `modelrepo.Provider`
implementations handle provider-side calls for Ollama/Ollama Cloud, vLLM,
OpenAI, Anthropic, Gemini, AWS Bedrock, and Vertex.
Runtime state catalogs configured backend capabilities for selectors and
diagnostics.

**Tool System** — chains invoke tools by name and resolution happens at
runtime. Built-in providers include `local_shell`, `local_fs`, `webtools`,
`echo`, `print`, OpenAPI-backed remote tools (third-party specs), and
MCP-backed tools. HITL policy wraps tool execution where approval is required.

**Event-driven async** — `libbus` abstracts the local event bus. Services
publish typed events such as `task.events.step_completed`, and other services
subscribe without direct package coupling.

**Key files to orient yourself:**

| File | What it shows |
|------|---------------|
| `internal/kernel/taskengine/tasktype.go` | Chain schema, task handlers, branch operators |
| `internal/kernel/taskengine/taskenv.go` | Runtime tool resolution and chain execution context |
| `internal/surfaces/contenoxcli/cli.go` | CLI dispatch |
| `internal/surfaces/contenoxcli/engine.go` | CLI-local engine bootstrap |
| `internal/surfaces/acpsvc/` | ACP session transport (editors and the terminal UI build on this) |
| `internal/kernel/agentinstance/` | the embeddable fleet kernel (missions run in-process) |

## Repository structure

The `contenox` binary is the only entrypoint. Current commands include
`setup`, `init`, `chat`, `run`, `tools`, `mcp`, `backend`, `agent`, `cache`,
`config`, `model`, `state`, `doctor`, `session`, `acp`, `acpx`, `workspace`,
`sandbox`, `shell-env`, `update`, and `version`. (`acp` and `acpx` speak stdio
protocols for editors; `workspace` grants or revokes the workspace roots a
session may run in; the rest work against the local database directly.)

The layout is layered, and the domain logic lives under Go `internal/` so
the compiler defines the public surface. The top-level packages are the
deliberate exceptions: `libacp/` (a reusable Go ACP implementation),
`libtracker/`, and the infrastructure leaves `libdbexec/`, `libbus/`,
`libkvstore/`, and `errdefs/` — all importable as
`github.com/contenox/contenox/<pkg>`. They are maintained here rather than in
separate repos so a clone of this repository builds with no dependency you
cannot fetch.

```text
cmd/contenox/            contenox binary entry point
internal/kernel/         the guts: taskengine, agentinstance, nativeturn,
                         enginesvc, reasoning, llmresolver, tools
internal/models/         model plumbing: modelrepo + provider drivers,
                         llmrepo, modelcapability, modelservice,
                         providerservice, runtimestate
internal/services/       domain services: chat, session, mission, fleet,
                         hitl, mcp, tools, shell, vfs, workspace, …
internal/store/          runtimetypes — entities + the SQLite Store
                         (including the model registry)
internal/surfaces/       thin adapters: contenoxcli (CLI), acpsvc (ACP
                         sessions), beamtui (the TUI, in development)
internal/libsandbox/     sandboxing, no LLM dependency
libbus/, libdbexec/, libkvstore/, libtracker/
                         top-level infrastructure libraries with no LLM
                         dependency
errdefs/                 shared error vocabulary (tiny leaf)
docs/rnd/                design records and R&D directions
website/                 contenox.com (Astro; renders docs/ as content)
```

The layering is a rule, not a convention: surfaces stay thin and call
services; business logic never lives in `internal/surfaces/`.

## Local development setup

Go 1.25+ and [Task](https://taskfile.dev) (`go install
github.com/go-task/task/v3/cmd/task@latest`). The CLI is pure Go — no C
toolchain needed.

```bash
task build        # build bin/contenox
task dev-link     # symlink it into ~/.local/bin
task --list       # everything else
```

Cutting an actual release (GitHub Release) is maintainer-only and documented
separately in
[docs/development/release-pipelines.md](docs/development/release-pipelines.md).

## Running tests

Before submitting a pull request, run the checks that match your change.

Fast path, matching CI:

```bash
task test-unit
```

Full Go suite, including any system tests that are not separately gated:

```bash
task test
```

Targeted system suites are explicit because some use local services or
containers:

```bash
task test-system
```

CLI package and help drift checks:

```bash
task test-cli-verbose
task test-cli-help
```

ACP wire-conformance harnesses (need the Rust reference peers built — see the
comments in Taskfile.yml):

```bash
task acp-conformance
task acp-client-e2e
task acp-host-e2e
```

Optional race detector:

```bash
go test -race ./... -run '^TestUnit_'
```

If command names, flags, README examples, or user-facing help changed, also
run `task test-cli-help` and update the relevant docs.

## Pull request guidelines

1. Open an issue first for major feature or architecture changes.
2. Branch from `main` with a descriptive name such as `feature/xyz`,
   `fix/abc`, or `docs/def`.
3. Use clear commit messages. Conventional Commit prefixes are preferred.
4. Run `gofmt` on Go changes.
5. Keep docs and blueprints in sync with public-surface changes.
6. Keep generated artifacts out of commits unless the release process requires
   them.

## Code conventions

### Go style

- Prefer self-documenting code. Add short comments only for non-obvious
  invariants, protocol behavior, security boundaries, or tricky edge cases.
- Service constructors accept interfaces. Wire concrete implementations in
  `internal/surfaces/contenoxcli/engine.go`.
- Keep chain behavior declarative. Business logic belongs in chain definitions
  unless a new primitive is genuinely needed in `taskengine`.
- Tool exposure must be explicit. `execute_config.tools` omitted, `null`, or
  `[]` exposes no registry tools. Use `["*"]` only when a chain intentionally
  opts into all registered tools, and prefer narrow allowlists such as
  `["local_fs"]`.
- Runtime allowlists may restrict task allowlists but must not expand them.
- Wide interfaces are a smell. New code should accept the narrowest interface
  slice it actually needs.
- Product language: enjoyable-first. Sensible, safe defaults without nagging —
  help text and errors teach the next step instead of lecturing.
