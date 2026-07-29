# Local Runtime

Contenox runs a local runtime process for the active workspace.

Run **Contenox: Run Setup** (or the Guided flow) once to choose the model provider and default model that the editor should use. The extension reuses the same local Contenox configuration as the CLI and will also refresh defaults via `init --update`.

The extension runs `contenox init --update` + setup wizard under the hood for you (no need to think about CLI).

In VS Code Remote Development, WSL, Dev Containers, SSH, and Codespaces, the runtime runs on the remote workspace host. Use a VSIX/Marketplace package that matches that remote host, or set `contenox.binaryPath` to a `contenox` binary inside the remote environment.

Useful follow-up commands:

- `Contenox: Run Setup`
- `Contenox: Doctor` (diagnostics + issues with hints)
- `Contenox: Show Status`
- `Contenox: Show Output`
- `Contenox: Select Provider`
- `Contenox: Select Chat Model`
