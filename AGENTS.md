# AGENTS.md

The `contenox` runtime: one Go binary, a CLI, ACP over stdio for editors, and a
terminal UI. `CONTRIBUTING.md` is the long form. This is the router.

## Never commit

Leave changes in the working tree for the maintainer to review. No `git commit`,
no `git add`, no `git push` — not even "just the formatting fix". If you spawn
subagents, pass this rule down.

## Read before you touch

Do not infer a subsystem's contract from its call sites. Read the doc first.

| You are touching | Read first |
|---|---|
| `internal/kernel/taskengine/` | `docs/specification/handlers.md`, `docs/specification/transitions.md` |
| `internal/surfaces/acpsvc/`, `libacp/` | `docs/development/acp-client.md`, `libacp/README.md` |
| `internal/services/hitlservice/`, approval policy, tool gating | `docs/guide/hitl.md` (the seven envelope presets), `docs/reference/config.md` |
| `internal/surfaces/beamtui/` | `internal/surfaces/beamtui/testkit/golden.go` — the golden-frame workflow |
| `internal/surfaces/contenoxcli/` | `docs/reference/contenox-cli.md`, `scripts/verify_cli_help.sh` |
| `internal/kernel/agentinstance/`, `internal/services/fleetservice/` | `docs/guide/concepts.md` |
| sandboxing, env scrubbing, workspace grants | `docs/guide/agent-sandbox.md`, `docs/guide/agent-threat-model.md` |
| bus events, `task.events.*` | `docs/development/engine-events.md`, `docs/guide/events.md` |
| the ACP conformance harnesses, `tools/acp-validator` | `docs/development/build-requirements.md`, `tools/acp-validator/README.md` |
| `internal/version/`, release workflows | `docs/development/release-pipelines.md` |
| **anything under `docs/`** | **`website/README.md` — see below** |

### `docs/` is the public website

`website/` renders `docs/` as contenox.com; the site owns no content of its own.
Editing a doc **is** editing the website. A dangling link is a shipped 404.

- Moving or renaming a doc changes a live URL. Add a redirect in
  `website/astro.config.mjs` or don't move it.
- Root-relative image paths are rewritten to the S3 bucket by
  `src/lib/remark-md-links.mjs`. New media filename → add it to that `S3_MEDIA` set.
- `website/public/install.sh` must stay URL-stable. `https://contenox.com/install.sh`
  is in the README, the docs, and every install instruction in the wild.
- Internal working notes never go in `docs/`. They go to the gitignored `.notes/`.

## Pre-commit gate

Run what matches your change. `gofmt` and `vet` always.

```bash
gofmt -l .            # must print NOTHING. Fix with: gofmt -w .
go vet ./...
task test-unit        # the fast gate CI runs
task test-cli-help    # if commands, flags, or help text changed
task website:deps && task website:build   # if you touched docs/ or website/
```

Touched a surface, a service boundary, or the engine:

```bash
task test             # full serial suite
```

The full release gate is `task test-all`. It runs nightly in
`.github/workflows/release-gate.yml`, not on every push.

CI additionally enforces `go vet ./...` and `govulncheck ./...`. If you add a
dependency, expect govulncheck to have an opinion about it.

## Rules that earned their place

- **Almost everything lives under `internal/`.** That is deliberate: the compiler
  defines the public surface. `libacp/` is the one intentional exception — a
  reusable Go ACP implementation, importable as
  `github.com/contenox/contenox/libacp`. It is maintained in this tree so the
  public repo builds from a clone with no private dependency. Do not add a second exception
  without the maintainer saying so.
- **Surfaces stay thin.** `internal/surfaces/` adapts; it never holds business
  logic. Surfaces call services. This is a rule, not a convention.
- **Session-level ACP features belong in `acpsvc`, never in the TUI alone.**
  Slash commands, session controls, mid-conversation affordances — implement them
  in `internal/surfaces/acpsvc` (registry: `commands.go`) so `contenox acp` and
  every ACP editor get them. `beamtui` is a consumer. Verify through
  `contenox acp` before calling it done.
- **Runtime allowlists may restrict a task's allowlist. They must never expand it.**
- **Tool exposure is explicit.** `execute_config.tools` omitted, `null`, or `[]`
  exposes no registry tools. Prefer `["local_fs"]` over `["*"]`.
- **Wide interfaces are a smell.** Accept the narrowest interface slice you
  actually need. Service constructors take interfaces; concrete wiring happens in
  `internal/surfaces/contenoxcli/engine.go`.
- **Keep chain behavior declarative.** Business logic belongs in chain
  definitions. A new Go primitive in `taskengine` should be rare and argued for.
- **Comments are godoc conventions plus invariants.** Identifier-first. Explain
  non-obvious invariants, protocol behavior, security boundaries, tricky edges —
  not what the next line does.
- **Public-surface change means a docs change in the same commit.** Command
  names, flags, help text, README examples.

## Traps

- **CI fails the build on `gofmt -l .`.** Run it before you finish; fix with
  `gofmt -w .`. An import-path rename (`github.com/contenox/libacp` →
  `github.com/contenox/contenox/libacp`) touched a large number of files — the
  imports themselves are fully migrated, but check formatting is clean before
  assuming the gate is green. Do not opportunistically reformat files you did
  not otherwise touch.
- **`libacp/` is in this tree and part of the main module** — it has no `go.mod`
  of its own and imports as `github.com/contenox/contenox/libacp`. It was
  briefly its own repo and was brought back so a clone of the public repo builds
  with no private dependency. The `acp-conformance` and `acp-client-e2e` targets
  point at `./libacp/...` and are correct as written.
- **The ACP harnesses need Rust reference peers** built from a
  `github.com/agentclientprotocol/rust-sdk` checkout. Absent peers skip or fail a
  precondition — that is not a contenox regression. See `tools/acp-validator/README.md`.
- **`website/README.md` still documents `make deps-website`.** The build system is
  Task now (`task website:deps`). The Makefile is gone.
- **Golden frames are checked in.** `-update` rewrites them. Regenerate only after
  you have looked at the diff and can say why it changed.

## Scope

Match the size of the work to the size of the ask. If you find a second bug while
fixing the first, say so in one line and let the maintainer decide — do not fix it
in passing. Verify before asserting: an assumption written into a brief executes
at scale.
