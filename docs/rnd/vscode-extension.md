---
title: "The Contenox VS Code extension"
description: An editor-native @contenox chat participant, autocomplete, and gated tool execution for VS Code and VSCodium — distributed through the Marketplace and remote dev hosts.
---

# The Contenox VS Code extension

The Contenox extension brought the runtime directly into VS Code and VSCodium as a local-first workspace extension: it bundled and started `contenox` for the active workspace, and registered itself as a native VS Code language-model vendor, so `@contenox` appeared in Copilot Chat's own model picker with no separate sign-in required for Contenox-owned requests. From the command palette or the lightbulb menu you could ask about a selection, explain or fix diagnostics in place, review the current diff, and draft a commit message from it — all backed by the same chains, models, and provider config the CLI used.

Inline autocomplete ran on a deliberately separate, FIM-capable model from chat, so a fast local coder model could drive ghost text while a larger hosted model handled conversation — configurable per workspace or globally via the CLI. Every tool call the extension's agent made — MCP, OpenAPI, or local shell — was gated by the same human-in-the-loop policies as everywhere else, surfaced as an approval the developer accepted or denied before it ran. The extension was fully remote-aware: over SSH, WSL, Dev Containers, and Codespaces it ran as a workspace extension on the remote host itself, so the runtime, its data directory, and its diagnostics travelled with the checked-out code rather than staying on the local laptop. Releases shipped as platform-specific VSIX packages (Linux, macOS, Windows, x64 and ARM64) through a dedicated release pipeline and the VS Code Marketplace.

![The Contenox extension icon](/vscode-extension-icon.png)

## What it proved

- **Editor-native distribution works without a separate account.** Registering as a real VS Code LM vendor put `@contenox` in the same picker as every other model, with zero extra sign-in for the runtime's own requests.
- **Splitting the chat model from the autocomplete model is a real, useful pattern.** A small local FIM model for ghost text and a larger hosted model for conversation, configured independently, is now how the runtime treats "chat vs. autocomplete" everywhere.
- **The local-first design travels onto remote dev hosts cleanly.** Running as a genuine workspace extension over SSH, WSL, containers, and Codespaces validated that the runtime doesn't assume it owns the developer's own laptop.
- **Gated tool execution belongs inside the editor's own surface**, not bolted on afterward — the same approval model now used by the CLI and every ACP session was proven here first.

## Where this lives now

Any ACP-speaking editor — Zed, JetBrains, and others — drives the identical session and approval machinery the extension proved out, through `contenox acp`.
