# Build requirements

What you need depends on what you're building. The `contenox` CLI needs almost
nothing; `modeld`, the native local-inference daemon, needs a full per-OS
native toolchain. This page is the one place that lists all of it, per
component and per platform.

Two build systems cover this repo, and they don't overlap:

| Build system | Covers | Entry point |
|---|---|---|
| [Task](https://taskfile.dev) | `contenox` CLI, website, VS Code extension, Go test suites | `Taskfile.yml` — run `task --list` |
| Make | `modeld` only (native build, package, release) | `Makefile` — run `make help` |

`modeld` is on Make, not Task, because it needs per-OS/per-device native
toolchains (cmake, a C/C++ compiler, OpenVINO GenAI, optionally CUDA/HIP) that
a plain Go toolchain does not provide, and because its release path assembles
platforms (Windows, macOS) that no single CI/dev box builds in one pass.

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
`vscode` job matrix in `.github/workflows/release.yml`, which builds it
natively per target for the extension bundle.

## Website (contenox.com)

- Node.js + npm
- `task website:deps`, `task website:dev`, `task website:build`

## VS Code extension

- Node.js 22 + npm
- Packaged natively per target, not cross-compiled — see the `vscode` job
  matrix in `.github/workflows/release.yml`: `linux-x64`, `linux-arm64`
  (both on `ubuntu-latest`), `darwin-arm64` (`macos-15`), `darwin-x64`
  (`macos-15-intel`), `win32-x64` (built on `ubuntu-latest` via Go cross-compile)
- Linux runners additionally need `unzip` and `file` (VSIX package inspection)
- `task vscode:deps`, `task vscode:check`, `task vscode:build`, `task vscode:package`

## `modeld` — the native local-inference daemon

Links llama.cpp via CGO and requires OpenVINO GenAI as a second backend — see
`cmd/modeld`, `internal/modeld`. A exception is
macOS, where OpenVINO GenAI isn't supported at all (see below).

Common to every platform:

- Go 1.25+ with CGO enabled (`CGO_ENABLED=1`)
- CMake
- A C/C++ compiler
- Python 3 (creates the OpenVINO venv)
- git (fetches the pinned llama.cpp commit and the matching OpenVINO GenAI
  C++ source checkout)
- OpenVINO GenAI SDK + C++ headers (`make deps-openvino`) — required except on macOS

```bash
make deps-modeld     # fetch llama.cpp ref source + OpenVINO GenAI SDK/headers
make build-modeld    # bin/modeld
make run-modeld      # build + serve
```

### Linux

- gcc/g++ (or clang) and CMake
- Python 3 + venv
- OpenVINO GenAI: `make deps-openvino` creates `.openvino/venv`, `pip install
  openvino openvino-genai`, and checks out the matching `openvino.genai` C++
  source tree
- Optional GPU accel, auto-detected from `PATH`: `nvcc` (CUDA Toolkit) enables
  the CUDA ggml backend plugin, `hipcc` (ROCm) enables HIP
- This is the only platform with a verified end-to-end dev loop
  (`make build-modeld`, `make package-modeld`)

### Windows

- MinGW-w64 (default) or MSVC/Clang (`MODELD_WINDOWS_TOOLCHAIN=msvc`) — either
  toolchain link path is supported by the release packager
- CMake
- Python 3 + venv (OpenVINO's Windows wheels)
- OpenVINO GenAI SDK for Windows
- git and a bash shell (Git Bash/MSYS) — the release scripts are bash
- `cygpath` (from Git Bash/MSYS) — used by `package-modeld-release-windows`
  to convert paths for the CGO compiler
- MSVC runtime redistributables (`msvcp140.dll`, `vcruntime140.dll`,
  `vcruntime140_1.dll`, `vcomp140.dll`) bundled alongside the binary —
  `vcomp140.dll` (MSVC OpenMP) is a load-time dependency of `ggml-base.dll`,
  so the daemon won't start without it
- Optional: NVIDIA CUDA Toolkit for GPU accel
- The plain dev targets (`build-modeld`, `package-modeld`) are Linux-oriented;
  on Windows use the full release flow instead:
  `make bundle-modeld-deps-windows` then
  `make package-modeld-release-windows MODELD_DEPS_ROOT=...`

### macOS (darwin)

- Xcode Command Line Tools (`clang`, the system C/C++ compiler)
- CMake
- **No OpenVINO GenAI** — macOS is llama.cpp + Metal only. OpenVINO GenAI is
  not supported on Apple Silicon in this build, so the darwin package never
  requires or links it (`MODELD_RELEASE_OPENVINO` is forced to `0` for this
  target)
- No CUDA/HIP (no equivalent GPU accel path on macOS)
- `make bundle-modeld-deps-darwin` then
  `make package-modeld-release-darwin MODELD_DEPS_ROOT=...`

## Optional tooling (release/CI machinery, not needed day-to-day)

- **AWS CLI** — only if pushing/pulling native dependency bundles or release
  archives to S3 (`MODELD_DEPS_S3_URI` / `MODELD_RELEASE_S3_URI`). A local
  directory works as a store for testing the whole push/pull/dedup flow
  without credentials.
- **Rust toolchain** — only for building the ACP conformance test peers
  (`tools/acp-validator`, `tools/rust-sdk`) used by `task acp-conformance` /
  `task acp-client-e2e` / `task acp-host-e2e`. These targets fail with a clear
  "set this env var" message rather than silently skipping when a peer binary
  isn't built.

## Maintainers: releasing

Everything above is what you need to *build*. Actually *releasing* — cutting
a GitHub Release, publishing to the VS Code Marketplace, or shipping a
`modeld` build — is a separate, maintainer-only concern with its own external
dependencies (an S3-backed store and `.env` for `modeld`, a Marketplace
secret for the extension). See
[release-pipelines.md](release-pipelines.md).

## See also

- [release-pipelines.md](release-pipelines.md) — maintainers: how each release actually ships, and when S3/secrets are involved
- [CONTRIBUTING.md](../../CONTRIBUTING.md) — local development setup and PR guidelines
- [Windows development](windows-development.md) — day-to-day Windows workflow for the CLI/runtime (not modeld)
