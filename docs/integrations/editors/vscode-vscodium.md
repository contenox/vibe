---
title: Use Contenox from VS Code or VSCodium
description: Install the Contenox extension from the Marketplace or a GitHub Release.
---

# Use Contenox from VS Code or VSCodium

Contenox ships a local-first editor extension for VS Code-compatible editors. The
extension starts the Contenox runtime directly from a bundled binary, so you do
not need `contenox` installed separately or a background daemon running.

In VS Code, install it from the Marketplace. For VSCodium, remote workspace hosts,
or offline installs, use the platform-specific `.vsix` from a GitHub Release. Both
paths are below.

---

## Install from the Marketplace

In VS Code, open the Extensions view, search for **Contenox**, and install it. Or
from the command line:

```bash
code --install-extension contenox.contenox-runtime
```

Then continue with [Set up Contenox in the editor](#4-set-up-contenox-in-the-editor).

---

## 1. Download the right VSIX

Open the latest Contenox release:

[github.com/contenox/contenox/releases/latest](https://github.com/contenox/contenox/releases/latest)

Download the extension package that matches the machine where the extension host
runs:

| Platform | Release asset |
|----------|---------------|
| Linux x64 | `contenox-runtime-linux-x64-<version>.vsix` |
| Linux ARM64 | `contenox-runtime-linux-arm64-<version>.vsix` |
| macOS Apple Silicon | `contenox-runtime-darwin-arm64-<version>.vsix` |
| macOS Intel | `contenox-runtime-darwin-x64-<version>.vsix` |
| Windows x64 | `contenox-runtime-win32-x64-<version>.vsix` |

For normal local editing, choose the package for your laptop or desktop. For
Remote SSH, Dev Containers, or Codespaces-style workspaces, choose the package
for the remote workspace host, because Contenox runs as a workspace extension.

---

## 2. Install in VS Code

From the command line:

```bash
code --install-extension ./contenox-runtime-linux-x64-<version>.vsix --force
```

Replace the filename with the file you downloaded.

Or use the UI:

1. Open Extensions.
2. Open the `...` menu.
3. Choose **Install from VSIX...**.
4. Select the downloaded `contenox-runtime-...vsix` file.

Restart or reload the window after installation.

---

## 3. Install in VSCodium

From the command line:

```bash
codium --install-extension ./contenox-runtime-linux-x64-<version>.vsix --force
```

Replace the filename with the file you downloaded.

Or use the UI:

1. Open Extensions.
2. Open the `...` menu.
3. Choose **Install from VSIX...**.
4. Select the downloaded `contenox-runtime-...vsix` file.

Restart or reload the window after installation.

---

## 4. Set up Contenox in the editor

Open a trusted workspace, then run these commands from the Command Palette:

```text
Contenox: Run Setup
Contenox: Restart Runtime
Contenox: Show Status
Contenox: Open Chat
```

The setup command configures the local Contenox state and model defaults. The
restart command restarts the bundled `contenox vscode-agent --stdio` process. The
status command confirms whether the runtime sees a provider and model.

Contenox also registers as a `contenox` vendor in VS Code's native language model
chat provider picker, so it appears alongside other model vendors when you switch
models in Copilot Chat. To check runtime health from the Command Palette, run
`Contenox: Doctor`.

---

## Autocomplete

The extension can provide inline ghost-text suggestions as you type. It is
disabled by default and uses a **separate, FIM-capable model** from chat.

Set the autocomplete model from the CLI (or with `contenox.autocompleteModel` in
VS Code settings):

```bash
contenox config set default-autocomplete-provider mistral
contenox config set default-autocomplete-model codestral-latest
```

Then run `Contenox: Enable Autocomplete`, followed by `Contenox: Test
Autocomplete At Cursor` to confirm the model returns usable text. With no
autocomplete model configured, no suggestions appear.

---

## Updating

Download the newer `.vsix` from the latest release and run the same install
command again:

```bash
code --install-extension ./contenox-runtime-linux-x64-<new-version>.vsix --force
codium --install-extension ./contenox-runtime-linux-x64-<new-version>.vsix --force
```

Your Contenox data directory is not deleted by reinstalling the extension.

---

## Uninstalling

```bash
code --uninstall-extension contenox.contenox-runtime
codium --uninstall-extension contenox.contenox-runtime
```

The extension ID is `contenox.contenox-runtime`.

---

## Troubleshooting

**The extension installs but the runtime does not start.** Check that you
downloaded the VSIX for the extension host platform. A macOS package cannot run
inside a Linux Remote SSH workspace.

**The command `code` or `codium` is not found.** Use the editor UI's **Install
from VSIX...** action, or enable the shell command from your editor's command
palette.

**I want to see what happened.** Run `Contenox: Show Output` and `Contenox: Show
Telemetry Log`.

**The release has no VSIX files yet.** Use a release that includes
`contenox-runtime-<target>-<version>.vsix` assets. Contenox release CI creates these
packages for Linux, macOS, and Windows targets.
