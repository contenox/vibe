# Contenox for VS Code

Run local, reviewable AI workflows in VS Code. Use it to:

- **Chat about your code** with `@contenox` — workspace questions and source-aware
  explanations, no GitHub Copilot sign-in required.
- **Fix problems** — explain or fix diagnostics from the lightbulb or command palette.
- **Review before you push** — summarize your workspace changes and draft a commit
  message from the current diff.
- **Get inline autocomplete** from a local or cloud model you choose (needs a
  separate autocomplete model — see [Autocomplete](#autocomplete) below).
- **Run tool workflows you approve** — MCP, OpenAPI, and shell tools gated by
  human-in-the-loop approval.

The extension is the VS Code frontend for the Contenox runtime: it uses the models
and providers you configure, keeps state on your machine, and asks for approval
before crossing policy boundaries.

## Why Contenox

Modern coding agents are useful, but too much AI-assisted work still depends on
fragile prompt habits, hidden context, vendor-specific behavior, and black-box
tool execution.

Contenox takes a different approach: repeated AI work should be defined,
inspected, versioned, and controlled like an engineering artifact. Contenox
Chains make prompts, model routing, tool access, retries, branches, budgets, and
approval gates reviewable.

Contenox is runtime-first, not editor-first: it sits across your editor,
terminal, project state, tools, sessions, and model/provider configuration.

## What You Can Do

- Chat with `@contenox` in VS Code Chat without requiring GitHub Copilot sign-in
  for Contenox-owned requests.
- Ask about selected code or open files with editor context attached.
- Explain and fix diagnostics from the command palette or lightbulb menu.
- Review workspace changes and draft commit messages from the current diff.
- Use inline autocomplete powered by a separate, FIM-capable autocomplete model
  you configure (see [Autocomplete](#autocomplete)).
- Connect local or external models, including local GGUF models and common cloud
  providers.
- Use MCP, OpenAPI, and local tools through Contenox runtime policies.
- Approve or deny gated tool actions before they run.
- Keep sessions, configuration, telemetry, and workflow state local.

## Local-First Ownership

The extension includes the Contenox runtime and starts it locally for the active
workspace. You can use the bundled runtime, point the extension at an existing
Contenox binary, or reuse an existing Contenox data directory.

In VS Code Remote Development, WSL, Dev Containers, SSH, and GitHub Codespaces,
the extension runs as a workspace extension on the remote workspace host. That
means the Contenox runtime also runs on the remote host, not on your local
laptop. Install the Marketplace/VSIX package that matches the remote operating
system and architecture, or set `contenox.binaryPath` to a `contenox` binary
that exists inside that remote environment.

Contenox is designed around:

- local-first usage
- model and provider choice
- auditable execution traces
- human-in-the-loop approval
- explicit tool and policy boundaries
- workflows that can graduate from chat into repeatable Chains

## Getting Started

1. Install the extension.
2. Run `Contenox: Run Setup` (or `Contenox: Doctor` to diagnose) from the command palette. The extension will run `init --update` automatically to keep your defaults/chains fresh, then guide provider + model selection.
3. Choose or configure a model/provider (or use the Guided flow).
4. Run `Contenox: Open Chat`.
5. Ask `@contenox` about your code, or use the editor actions (`Ask About
   Selection`, `Explain Diagnostics`, `Review Workspace Changes`, `Draft Commit
   Message`) from the command palette and lightbulb menu.

## Autocomplete

Contenox can provide inline ghost-text suggestions as you type. It is disabled
by default; enable it after choosing an autocomplete model.

Autocomplete uses a **separate model from chat**. Use a FIM/coder model when
you can; the autocomplete chain sends prefix/suffix context and asks the model
to fill the middle. This is useful when you want low-latency ghost text without
changing the model used for chat and agent workflows.

```sh
# Chat can stay hosted:
contenox config set default-provider openai
contenox config set default-model gpt-5-mini

# Ghost text can run locally through modeld:
contenox config set default-autocomplete-provider llama
contenox config set default-autocomplete-model qwen3-coder-30b-a3b

# Or on OpenVINO/modeld:
contenox config set default-autocomplete-provider openvino
contenox config set default-autocomplete-model qwen2.5-coder-1.5b-ov

# Or on another code model:
contenox config set default-autocomplete-provider ollama
contenox config set default-autocomplete-model qwen2.5-coder:7b
contenox config set default-autocomplete-provider mistral
contenox config set default-autocomplete-model codestral-latest
```

or override it per workspace with `contenox.autocompleteModel` /
`contenox.autocompleteProvider`. If you leave these empty, Contenox uses its
runtime defaults and may auto-route to a configured code backend. Set them
explicitly when you want predictable autocomplete behavior.

Enable autocomplete from the status bar or with `Contenox: Enable Autocomplete`.
If nothing shows up after enabling it, run `Contenox: Test Autocomplete At Cursor`
— it reports whether the model returned usable text.

## Remote Development

Contenox declares itself as a VS Code workspace extension so workspace files,
git state, terminals, tools, and the Contenox runtime live on the same machine as
the checked-out code.

Remote behavior to expect:

- SSH, WSL, containers, and Codespaces need a Linux-compatible runtime in the
  remote environment.
- `Contenox: Run Setup` opens a terminal in the remote workspace.
- `contenox.dataDir` is resolved in the remote environment when set.
- Local diagnostic telemetry is written where the runtime runs.
- Use `Developer: Show Running Extensions` to confirm Contenox is running in the
  remote extension host.

If the runtime cannot start, run `Contenox: Show Output`; the log includes the
remote name, platform, architecture, runtime path, and the exact spawn error.

## CLI Compatibility

If you already use Contenox from the terminal, the extension can reuse the same
state and configuration. Common settings are available through the CLI:

```sh
contenox config set default-provider mistral
contenox config set default-model codestral-latest
contenox config set default-autocomplete-provider mistral
contenox config set default-autocomplete-model codestral-latest

# Mix hosted chat with local autocomplete:
contenox config set default-provider openai
contenox config set default-model gpt-5-mini
contenox config set default-autocomplete-provider llama
contenox config set default-autocomplete-model qwen3-coder-30b-a3b
```

## Links

- Website: https://contenox.com
- Runtime source: https://github.com/contenox/contenox
- License: Apache 2.0
