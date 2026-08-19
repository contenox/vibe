# Contributing to Contenox

Thanks for helping improve Contenox. This repository is the contenox agent server: ACP over stdio for editors
(Zed, JetBrains, AionUi, OpenClaw), `contenox serve` for a paired host, and
the CLI that declares, inspects and fires agents. Why the surface looks like
this: see WHY.md.

## Code of Conduct

Treat contributors with respect. Keep technical disagreements concrete and
actionable.

## Architecture

Contenox is a single-binary agent server. It hosts agents and sessions and
brings no tools of its own: the operator supplies them, from an ACP client or
from MCP servers they attach.

```text
CLI / ACP stdio sessions / relay
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

- `contenox beam` — the terminal UI, and what bare `contenox` opens on a TTY
- `contenox run` — one task for a caller that is a program; `git diff | contenox
  "…"` is the same shape with stdin attached
- `contenox chat` — a session-backed conversation without the TUI
- `contenox acp` / `contenox acpx` for ACP editors
- `contenox serve` / `contenox pair` for a relay-reachable host
- the rest of the CLI (agents, missions, approvals, inbox, sessions, config,
  backends, models, tools, MCP, events, hitl, vet)

When you change this surface, update the relevant user docs **and the command's
own help** in the same change. The help text is the documentation most people
read, and it is what rots first: it has advertised a removed chat mode, called
`beam` an alias for the host, and pointed at a `contenox index` that no longer
existed — each time because a change landed without it.

### What an operator authors

Three files, and nothing else, decide what an agent is and what it may do. A
change to any of them is a change to the product's contract:

- **`agents/**.md`** — the agent harness declaration: prompt, `tools`,
  `posture`, model. `tools` is reach, not permission: `*` means every connected
  toolset, `!name` removes one, a bare name grants one, and `mcpServers` /
  `remoteTools` grant themselves by being declared.
- **`agents.toml` `[envelopes.<name>]`** — the permission layer. An envelope
  transpiles to a HITL policy; nobody hand-writes the JSON. It states
  allow/ask/deny per capability axis, and for an ask the wait itself
  (`timeout = "30m"`, `on_timeout`, or `timeout = "never"`).
- **`.generated/`** — output. Chains and policies land here and are rewritten
  on the next run; editing them is editing a build artifact.

Anything under `.generated/` that an operator must edit by hand is a bug in the
declaration format, not a workflow to document.

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
runtime. Built-in toolsets are `local_fs` and `local_shell`, both
forwarded to the ACP client; everything else is MCP-backed or an
OpenAPI-backed remote tool the operator attaches. HITL policy wraps tool execution where approval is required.

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
| `internal/surfaces/acpsvc/` | ACP session transport (editors build on this) |
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
                         sessions), fleetboot (mission host bootstrap)
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

Test names carry their tier, and the tasks select by it:

| Prefix | Means | Task |
| --- | --- | --- |
| `TestUnit_` | isolated, no I/O, fast inner loop | `task test-unit` |
| `TestIntegration_` | real dependencies, cross-package | `task test-integration` |
| `TestSystem_`, `TestE2E_` (also `TestHostE2E_`, `TestFleetE2E_`) | whole workflows | `task test-system` |

`-short` is not a release gate. It means *unit only*: it switches off every
case that needs a container, a kernel feature, a peer binary or a spawned
`contenox`, which is most of what the product is. Use it for the inner loop,
never as the thing you ship on.

```bash
task test-unit          # fast inner loop, TestUnit_ under -short
```

The release claim, and what CI runs on every push and pull request:

```bash
task test-system        # TestSystem_/TestE2E_, never -short
task test-integration   # TestIntegration_, against real dependencies
```

Both run under `-v` with `-count=1` and end with a census naming every case
that did **not** run, because `go test` reports a skipped package as `ok`:

```
==> test-system: 96 passed, 29 SKIPPED (not passed)
    did not run: TestSystem_BedrockCatalog_RegisteredAndChatCapable
    ...
```

Read that census. A case that skipped for want of Docker, a Landlock kernel or
a peer binary is not a case that passed. `SKIP=<regexp>` excludes cases by name
if you need to; CI uses it only to keep the multi-GB Ollama and vLLM model
pulls out of the per-push run, and the nightly release gate passes nothing so
everything is attempted.

Several hundred tests still carry names that predate the convention
(`TestLoopback_`, `TestManager_`, `TestFleetService_`, …). `task test-rest`
runs exactly those, so `task test-all` covers the whole tree without running
anything twice. `task test` remains the plain full sweep:

```bash
task test-rest          # what the named gates do not select
task test               # every test in the tree, whatever its name
task test-all           # the one-command release gate: all of the above, in CI's order
```

New tests take the tier their dependencies put them in. A `TestUnit_` that
asserts the implementation matches itself proves nothing a release can lean on;
if the behaviour you changed is reachable from the CLI, write a `TestSystem_`
that drives it.

Neither those suites nor an end-to-end run you drive by hand needs a live model.
The `scripted-test` backend ships in the ordinary binary and replays a JSON
dialog in place of the model, so the chain engine, tool dispatch, the HITL gate,
sessions and `beam` all keep running for real:

```bash
contenox backend add scripted --type scripted-test --script ./dialog.json
contenox config set default-provider scripted-test
contenox config set default-model scripted-test
```

The script format and how turns are consumed are in
[docs/development/scripted-test-backend.md](docs/development/scripted-test-backend.md).
It exercises the machinery, not the agent's judgement: a scripted run cannot
tell you whether a real model would have picked that tool, so behaviour changes
still have to be tried against one.

### End-to-end tests are written in another language, on purpose

A test written in the language of the thing it tests can cheat: it can reach
past the surfaces, construct internals, and swap a fake in where a user could
not. The e2e suites are therefore **not** Go. They build the shipped binary and
drive it from outside, asserting only on what an integrator can observe —
stdout, stderr, exit codes, files the run wrote, and state read back through
the product's own commands (`approvals list`, `mission show`, `session list`).

Reading the SQLite file directly is the same cheat wearing a different hat, and
is not allowed. If a behaviour can only be observed through internal state,
that is a finding — the product cannot show a user something it does — and it
belongs in an issue, not in a test that opens the database.

That suite lives in [`tools/contenox-e2e`](tools/contenox-e2e/README.md), a
Rust crate that builds `bin/contenox`, runs it in a scratch `HOME`, and reads
every durable fact back through a contenox command. A new e2e case joins it
rather than becoming a Go test that happens to exec something:

```bash
task e2e-cli                        # builds the binary, then runs the suite
task e2e-cli -- beam_under_a_pty    # anything after -- goes to cargo test
```

Its README covers how to write a case: the hermetic `Instance`, the typed
scripted-test dialog, the command and pty drivers, and the read-back helpers.
It needs a Rust toolchain, so `task test-all` skips it loudly when there is
none — read that notice like any other `SKIPPED, not passed`.

CLI package and help drift checks:

```bash
task test-cli-verbose
task test-cli-help
```

The opt-in storage backends, if you touched anything they run through. Each
starts a throwaway container, so you only need Docker to exercise this path:
without a container engine the task prints `SKIPPED, not passed` and exits 0
instead of a green `ok` you could mistake for a real run.

```bash
task test-backends          # all of the below
task test-store-postgres    # the store suite on Postgres
task test-bus-nats          # the libbus conformance suite, NATS included
task test-kv-valkey         # libkvstore on Valkey
task test-substrate         # the wiring that selects them, against real servers
```

CI runs those same tasks with `CONTENOX_TEST_REQUIRE_POSTGRES`,
`LIBBUS_REQUIRE_NATS` and `LIBKVSTORE_REQUIRE_VALKEY` set, which turns every
skip in them into a failure. Set them locally to demand the same of your own
machine.

ACP wire-conformance harnesses (need the Rust reference peers built — see the
comments in Taskfile.yml):

```bash
task acp-conformance
task acp-client-e2e
task acp-host-e2e
```

The published JSON Schemas under `schema/` are generated from the Go types that
load the formats — `hitlservice.Policy` and `taskengine.TaskChainDefinition`,
their doc comments becoming the schema descriptions. Every policy contenox
emits stamps them by URL, so a stale file is drift an operator's editor
validates against. Touch either type or its doc comments and regenerate:

```bash
task spec:generate   # rewrite schema/*.schema.json from the Go types
task spec:verify     # what CI runs: regenerate to a temp dir, fail on drift
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
6. Commit `schema/*.schema.json` when you regenerate it — it is published at
   contenox.com/schema/ and CI's `task spec:verify` fails when it is stale.
   Keep other generated artifacts out of commits unless the release process requires
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
