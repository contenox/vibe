# VS Code Marketplace Release Blueprint

Target extension ID: `contenox.contenox-runtime`

The VS Code extension is a thin adapter around the existing Contenox Go runtime.
Each Marketplace package should be platform-specific and contain exactly one
native binary:

```text
bin/contenox       macOS/Linux
bin/contenox.exe   Windows
```

## Identity

- Publisher: `contenox`
- Extension name: `contenox-runtime`
- Extension ID: `contenox.contenox-runtime`
- Display name: `Contenox`
- Homepage: `https://contenox.com`
- Repository: `https://github.com/contenox/contenox`
- Stable package: `packages/vscode/contenox-runtime-<target>-<version>.vsix`
- Proposed local package: `packages/vscode/contenox-runtime-<target>-<version>-proposed.vsix`

Do not publish under rejected or legacy IDs such as `contenox.runtime` or
`contenox.contenox`.

## Publisher Profile Copy

Use this for the Visual Studio Marketplace publisher profile description:

```text
Use Contenox for local, reviewable AI work in your editor and terminal: ask
about a codebase, review a diff before pushing, fix diagnostics, draft commit
messages, run approved tool workflows, and turn repeated work into reusable
Chains.

Contenox is an open-source, local-first AI workflow runtime for developers. It
keeps sessions, configuration, telemetry, and workflow state on your machine
while letting you choose local or hosted models and define explicit tool and
approval boundaries.

The VS Code extension brings the runtime into the editor for codebase chat,
code actions, autocomplete, review workflows, and human-in-the-loop tool use.
The same workflows can also run from the CLI or any ACP-compatible client, so
useful AI work can graduate from chat into versioned, auditable automation.
```

## GitHub Setup

Create a protected GitHub Environment:

```text
vscode-marketplace
```

Recommended protection:

- require manual reviewer approval before deployment
- restrict deployment branches/tags after the process is stable

Add this repository secret:

```text
VSCE_PAT
```

`VSCE_PAT` is transitional. Keep the publishing step isolated so it can move to
Microsoft Entra/workload identity before global Azure DevOps PAT retirement.

## Workflows

Marketplace publishing
should eventually be entered only through a promotion-gated release path, not by
manually pushing a `v*` tag.

`.github/workflows/vscode-extension-ci.yml` should run on extension and
`vscodeagent` changes. It should type-check the extension, build a Linux smoke
VSIX, verify package contents, smoke `bin/contenox --version`, and upload the
artifact.

`.github/workflows/vscode-marketplace.yml` should run on release tags and manual
dispatch. All target VSIX artifacts must build and pass package checks before
the final publish job starts.

Initial target matrix:

- `linux-x64`
- `linux-arm64`
- `darwin-arm64`
- `darwin-x64`
- `win32-x64`

Local target package smoke:

```sh
CONTENOX_VSCODE_TARGET=linux-x64 npm run package
CONTENOX_VSCODE_TARGET=linux-x64 npm run package:check -- artifacts/contenox-runtime-linux-x64-<version>.vsix
```

Remote Development and Codespaces use the workspace extension host. Contenox is
a workspace extension, so the bundled runtime must match the remote host, not
the user's desktop. A macOS desktop connected to a Linux SSH host, container, or
Codespace must install/run a Linux-compatible Contenox runtime in that remote
environment.

## First Publish Steps

1. Create the Visual Studio Marketplace publisher with ID `contenox`.
2. Confirm `packages/vscode/package.json` resolves to `contenox.contenox-runtime`.
3. Add `VSCE_PAT` as a GitHub Actions secret.
4. Create and protect the `vscode-marketplace` GitHub environment.
5. Run `VS Code Marketplace` manually with `publish=false`.
6. Download and inspect all target VSIX artifacts.
7. Install and smoke-test at least `linux-x64`, `darwin-arm64`, and `win32-x64`.
8. Run the workflow manually with `publish=true` and `pre_release=true`.
9. Install from Marketplace and smoke setup, chat, autocomplete, and telemetry.
10. Publish stable from a release tag after the pre-release is clean.

## Release Checklist

- [ ] Publisher ID is `contenox`
- [ ] Publisher profile description uses the approved local-first workflow copy
- [ ] Extension ID is `contenox.contenox-runtime`
- [ ] Display name is `Contenox`
- [ ] `private` is absent from `packages/vscode/package.json`
- [ ] `pricing` is `Free`
- [ ] Marketplace icon is PNG
- [ ] README has no arbitrary SVG images or `http://` links
- [ ] `LICENSE`, `CHANGELOG.md`, `SUPPORT.md`, and `SECURITY.md` are packaged
- [ ] each target VSIX contains exactly one native binary
- [ ] Unix target binary is executable
- [ ] Windows target binary is `bin/contenox.exe`
- [ ] no `src/**`, `node_modules/**`, source maps, `.env`, or generated VSIXs
- [ ] stable package has no `enabledApiProposals`
- [ ] setup works from the bundled binary alone, with no separate install
- [ ] remote SSH/container/Codespaces smoke confirms Contenox runs in the
      workspace extension host
- [ ] remote package/runtime target matches the remote host platform and
      architecture
- [ ] file/tool/shell mutations require approval
- [ ] output and telemetry are local and inspectable
- [ ] `vscode-marketplace` environment requires approval
- [ ] build jobs complete for every target before publish starts
- [ ] follow-up exists to migrate from PAT to Entra/workload identity

## Automated Tests To Add

- Remote extension-host smoke with an installed VSIX in SSH/container/Codespaces
- Bridge JSON-RPC smoke for initialize, health, shutdown, and multiline payloads
- Generated VSIX install smoke in a temporary VS Code profile
- Generated VSIX install smoke in Remote SSH, WSL/container, or Codespaces
- Runtime config smoke using a temporary data directory
- Cross-platform `bin/contenox --version` smoke
- Maintainer-only provider integration tests using scoped secrets

Do not run real provider/API-key tests in public PR CI.

## Automated Tests Added

- Extension Development Host smoke through `@vscode/test-cli` and
  `@vscode/test-electron`
- Contribution checks for command registration, workspace extension kind,
  walkthrough metadata, welcome content, and missing command-title duplication
