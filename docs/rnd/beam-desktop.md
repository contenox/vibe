---
title: "Beam Desktop: the client as a peer"
description: An Electron shell that deleted HTTP from the loading path entirely — window loaded from disk, runtime spawned as a child process, and one NDJSON pipe carrying both ACP and a private editor method family.
---

# Beam Desktop: the client as a peer

Where [the original Beam](/docs/rnd/beam-web/) was a web app served *by* the runtime, Beam Desktop inverted the relationship: the client became the host. The window was loaded from disk over `file://` — the nginx config deleted, the `contenox serve` embed path retired — and the runtime was spawned as a child process and spoken to over stdio. No port, no localhost binding, no HTTP server anywhere in the loading path. The codec was NDJSON, one JSON-RPC message per line, which is exactly what `libacp` already reads and writes; the desktop shell added no transport of its own.

The renderer had no Node access at all — `sandbox: true`, `contextIsolation: true`, `nodeIntegration: false` — and reached the runtime through a preload bridge exposing exactly four things: `send`, `onMessage`, `onClose`, `onStderr`. No filesystem, no child processes, no way to name the binary being executed. Owning the child process is also what made ACP's `env_var` authentication method usable for the first time: "relaunch me with these variables" can only be answered by whoever spawned the process. The variables the renderer could inject were allowlisted by prefix — `CONTENOX_*` and `*_API_KEY` — deliberately not taken from the runtime's own advertisement, since that advertisement arrives over the same bridge the renderer talks on and a compromised renderer must not be able to set `PATH` on a binary the main process spawns.

![The Beam Desktop shell: an empty chat pane, a workspace-roots rail listing two projects, and a status pill in the corner reporting the runtime connection](/lab-beam-desktop-shell.png)

*The shell against a runtime that had no default model configured. The status pill reports the pipe, not the engine — one of the defects that made the case for finishing the capability contract before building a client on top of it.*

The architectural centerpiece was **one bus**. `editorbus` re-hosted a recovered VS Code agent's dispatch onto the NDJSON pipe, and `contenox acp` handed `libacp` a multiplexer rather than raw stdio: it read the pipe once, answered the editor method family itself — workspace roots, git status and diff and per-hunk staging, cancellable search, host terminals, model and provider catalogs, config, missions, inbox — and forwarded everything else verbatim. ACP methods, notifications, responses, and unknown methods all passed through untouched, so a `methodNotFound` came from `libacp`'s own dispatcher rather than a silent drop. `initialize` was never claimed by the editor half: a second definition would fork the protocol on a single connection. Verified against the real `contenox acp` binary over one connection — ACP initialize at protocol version 1, an editor autocomplete dispatched, a mission list, editor health, fifteen commands, and an unknown method correctly answered `-32601`.

## What it proved

- **A desktop client can be a peer of the runtime rather than a payload it serves.** Removing HTTP from the loading path removed an entire class of question — which port, which origin, which cookie, which handshake — rather than answering it better.
- **One stdio pipe carries a standard protocol and a private method family without forking either.** ACP becomes the part of the vocabulary that happens to be standard, not the ceiling on what the connection can express.
- **The spawning process is the only place an env-var auth method can be honoured**, and the allowlist for it belongs on that side, never in the advertisement the renderer relays.
- **A client cannot be finished before the contract it consumes is.** Roughly 28 editor-bus methods were reachable from this shell and from no other surface, while the terminal UI reached around ACP into services directly — including a second, independent file browser. Every capability had two homes and neither was canonical. No amount of work in the shell could fix that, because the defect was underneath it.

## Where this lives now

The shell itself was not kept: about 42,800 lines of TypeScript, generated in roughly a day, past the point where it could be reviewed and therefore owned. What it surfaced was kept and is in the runtime today — a ConPTY backend that took shell sessions on Windows from absent to working for the terminal UI and ACP alike, session vitals promoted from a TUI widget into a runtime service, and the git review service.

The finding outlives the client. `acpsvc`'s editor bridge is the pattern the consolidation pointed at: a capability is defined once in ACP core and re-exported to the editor family, so the terminal UI and any editor are the same kind of consumer. That seam is the work; the next client gets written by hand, small, against a contract that has stopped moving.
