# Windows Development

This guide covers working on the Contenox runtime when your daily driver is Windows.

The codebase is primarily developed on Linux. Windows support exists for the pure-Go `contenox` CLI and most runtime packages.

## Recommended paths

| Path | Best for | Limitations |
|------|----------|-------------|
| **WSL2 (Ubuntu/Debian recommended)** | Day-to-day Go development, CLI work, running tests | Windows-specific runtime behavior (shells, paths) is not exercised. |
| **Native Windows (PowerShell/cmd + Git Bash)** | Verifying Windows-specific behavior: paths, `local_shell`, PowerShell/cmd.exe execution, terminal behavior of the terminal UI | `task` may be unavailable; invoke `go build`/`go test` directly. |
| **Remote SSH** | Offloading heavy work (gopls, builds) to a Linux box while editing on Windows | — |

For most contributors: install WSL2 and do the normal Linux flow inside it (see [CONTRIBUTING.md](../../CONTRIBUTING.md)). Drop to a native Windows checkout when you need Windows-specific verification.

## WSL2 setup (fast path)

1. Enable WSL2 and install a distro (Ubuntu 22.04/24.04 works well).
2. Inside WSL, follow the Linux prerequisites and build instructions from `CONTRIBUTING.md`.
3. Your Windows files are at `/mnt/c/...`. Clone the repo inside WSL for best performance on Go modules and builds.

## Native Windows prerequisites

- **Git for Windows** (brings Git Bash). Configure `core.autocrlf=input` (or `false`) to keep the repo LF-only:
  ```powershell
  git config --global core.autocrlf input
  ```
- Go 1.25+ (official MSI).
- [Task](https://taskfile.dev) is helpful but not required. You can get it via Chocolatey (`choco install go-task`) or just invoke the individual `go` commands.
- (Optional) Windows OpenSSH server so you can drive the box from your Linux dev machine.

Build the pure-Go CLI any time:

```powershell
go build -o bin\contenox.exe .\cmd\contenox
```

Or use the cross target (works from any OS):

```bash
task build-windows
```

## Building and testing on Windows

- Unit tests: `go test -short ./...` (or `task test-unit` if Task is present).
- Many system tests assume Unix shells; expect differences in `local_shell`, path handling, and process execution. Run real Windows verification with `cmd.exe` / PowerShell.
- Local model inference on Windows means a local Ollama install (or a reachable vLLM endpoint) — nothing extra to build.

## Common Windows gotchas

- **Shell execution**: `local_shell` and similar tools behave differently under `cmd.exe` vs PowerShell vs Git Bash. Test real Windows scenarios.
- **Paths**: Forward and backslashes are usually tolerated, but be careful with any code that shells out.
- **Line endings**: The repository uses LF. Let Git handle conversion on checkout.
- **Case sensitivity**: Windows is case-preserving but not sensitive; avoid relying on case-only differences in filenames.
- **Antivirus / real-time protection**: Can interfere with builds touching many small files during `go build`.

## Windows product surface

On Windows, Contenox is the same terminal-first product as everywhere else:
the `contenox` CLI, the terminal UI, and ACP editor integrations. There is no GUI
app and no Store package — the terminal is the product.

The CLI is pure Go (`CGO_ENABLED=0`) and cross-compiles cleanly
(`task build-windows` and the release workflow produce
`contenox-windows-amd64.exe`); `contenox update` has Windows-specific
replacement logic; `local_shell` detects `pwsh` / PowerShell / `cmd.exe` and
uses the correct invocation flags; local inference means Ollama on Windows or
a reachable vLLM endpoint; distribution is GitHub Releases (raw `.exe`,
`install.sh` covers Linux/macOS only).

## See also

- [CONTRIBUTING.md](../../CONTRIBUTING.md) — main local development instructions
