---
title: "Beam: the original web UI"
description: The React admin and chat SPA that first carried the Beam name — served from a single Go binary, with a diff-backed approval gate and a live agent workspace view.
---

# Beam: the original web UI

Beam was the browser face of Contenox: a React single-page app embedded directly in the `contenox` binary and served by `contenox serve`, at `127.0.0.1` by default, reading and writing the same SQLite state as the CLI. Nothing was hosted — one command opened the same sessions, chains, models, and HITL policies you already drove from the terminal, now in a window you could watch. A session sidebar grouped conversations by project; each session carried its own model, HITL policy, reasoning-effort, and token-limit controls, so one tab could run a strict local model while another ran a permissive hosted one, side by side.

Its centerpiece was the supervise-review-intervene loop made visible: a live workspace file tree showing exactly which files an agent's policy allowed it to touch, every tool call rendered as its own card, and — before any gated write or shell command ran — an inline diff you approved or rejected in place. A command palette (⌘/Ctrl-K) reached every action without leaving the keyboard. Behind the chat sat a full admin control plane: backends and model registry, a visual chain editor with a dagre-laid-out workflow graph, HITL policy editing, remote MCP/OpenAPI tool registration, and a fleet/mission board for dispatching and watching unattended runs. Authentication was a cookie-based BFF: the browser never touched the bearer token directly, sessions rode an `HttpOnly`, `SameSite=Strict` cookie minted from it, and the `/acp` chat WebSocket enforced the same credential in its handshake before ever upgrading the connection.

<video src="/beam-demo.webm" poster="/beam-video-cover.png" controls muted playsinline style="width:100%;border-radius:8px"></video>

*A chat with a registered agent in a project workspace: the agent reads the files, a gated write pauses at the inline permission card, one click allows it.*

![Beam's login page: a single access-token field gating the whole UI](/beam-login.png)

![A Beam session bound to a project workspace, with per-session Model, HITL Policy, Think, and Workspace controls above an empty chat](/beam-new-chat.png)

The companion backend manager configured providers, declared models with their capability flags, and wired backends into pools — all from the browser:

<video src="/backend-manager-demo.mp4" controls muted playsinline style="width:100%;border-radius:8px"></video>

## What it proved

- **A rich admin SPA can ship from one static Go binary with zero hosting.** No separate frontend deploy, no API gateway — `contenox serve` was the whole stack.
- **Diff-first, inline approval is the right shape for human-in-the-loop.** The approve/reject card pattern proved out here now anchors HITL everywhere else in the runtime.
- **Cookie-based BFF auth plus handshake-gated WebSockets is a workable remote-access model** for a local-first tool that still needs to leave loopback safely.
- **Visual chain editing over a graph layout is legible.** The dagre-based workflow visualizer showed that a chain's branches and retries read better as a graph than as raw JSON.

## Where this lives now

The Beam name lives on as `contenox beam`, the terminal UI — the same supervise-review-intervene loop, same HITL policies, no browser required.
