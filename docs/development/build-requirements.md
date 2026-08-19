# Build requirements

What you need depends on what you're building. The `contenox` CLI needs
almost nothing. This page is the one place that lists all of it, per
component and per platform.

This repo (`contenox`) is covered by one build system:

| Build system | Covers | Entry point |
|---|---|---|
| [Task](https://taskfile.dev) | `contenox` CLI, website, Go test suites | `Taskfile.yml` — run `task --list` |

## Baseline, for any contribution

- Go 1.25+
- [Task](https://taskfile.dev) (`go install github.com/go-task/task/v3/cmd/task@latest`)
- git

## `contenox` CLI

Pure Go, `CGO_ENABLED=0`. No C toolchain needed.

```bash
task build           # bin/contenox, host platform
task build-windows   # cross-compiled bin/contenox-windows-amd64.exe from any OS
```

The CLI cross-compiles cleanly to linux/darwin/windows, amd64/arm64 — see the
`build` job matrix in `.github/workflows/release.yml`.

## Website (contenox.com)

- Node.js + npm
- `task website:deps`, `task website:dev`, `task website:build`

## Optional tooling (release/CI machinery, not needed day-to-day)

- **Rust toolchain** — for two things, both of them tests that must not be
  written in Go:
  - `task e2e-cli`, the black-box CLI suite in
    [`tools/contenox-e2e`](../../tools/contenox-e2e/README.md). It builds the
    shipped binary and drives it from outside the module. Named by hand it
    fails without Rust; inside `task test-all` it skips loudly.
  - the ACP conformance test peers (`tools/acp-validator`, `tools/rust-sdk`)
    used by `task acp-conformance` / `task acp-client-e2e` /
    `task acp-host-e2e`. These targets fail with a clear "set this env var"
    message rather than silently skipping when a peer binary isn't built.

## Maintainers: releasing

Everything above is what you need to *build*. Actually *releasing* — cutting
a GitHub Release — is a separate, maintainer-only concern. See
[release-pipelines.md](release-pipelines.md).

## See also

- [release-pipelines.md](release-pipelines.md) — maintainers: how each release actually ships
- [CONTRIBUTING.md](../../CONTRIBUTING.md) — local development setup and PR guidelines
- [Windows development](windows-development.md) — day-to-day Windows workflow for the CLI/runtime
