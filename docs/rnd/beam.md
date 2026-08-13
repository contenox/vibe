---
title: "beam: the terminal client"
description: The terminal UI built inside contenox to test and harden the system — what it was, what it proved, and why it is being extracted into a client of its own rather than kept as a surface.
---

# beam: the terminal client

`beam` is the full-screen terminal UI reached with `contenox new` and
`contenox resume`: a session with chat, plan, shell and file edits in one
scrollback, an approval card that blocks a gated tool call inline, a command
palette with argument completion, `@` file addressing, and a status bar
carrying the live model, session and context pressure.

![beam running a real session: the brand header with model and session id, the
`/ @ ! ?` affordances, an operator prompt asking what is needed to take Stripe
checkout live, and the agent's `local_fs.read_file` calls resolving inline as
it reads the setup doc and the deployment manifest before answering](/lab-beam-session.png)

It was built inside the runtime as a surface, and used as one. Its real job
turned out to be different: it was the instrument that exercised every seam of
the system from a client's side, and the thing that answered a product question
nobody had asked directly.

## What it proved

- **The seam is the protocol, not the surface.** Of beam's own packages —
  `frame`, `input`, `keymap`, `style`, `term`, `textwidth`, `sanitize`, and the
  `brand`, `composer` and `picker` components — none imports anything from
  contenox. `palette` and `transcript` import only `libacp`. Roughly 7,600
  lines of it were already a general Agent Client Protocol client that had
  never been asked to be one.
- **Where the coupling actually was.** Four packages reach contenox services
  directly — `fileaddr` → `vfs`, `statusbar` → `sessionvitals`, `approval` →
  `approvalflow`, `app` → `sessionvitals` — and `enginebridge` is the adapter
  that carries all of it: 997 lines in `bridge.go` against `enginesvc`,
  `approvalflow`, `shellsession` and `vfs`. The other 720 lines of
  `enginebridge`, the event vocabulary, are a model of the wire rather than of
  contenox.
- **The two services with no ACP equivalent are the same two blocking
  everything else.** `sessionservice` and `sessionvitals` are what the terminal
  UI reaches for and the protocol does not carry. They are the whole gap
  between "beam is a contenox surface" and "beam is an ACP client".
- **Argument completion belongs to the protocol, not the client.** Beam's
  command palette fills its value domains from the session config selects the
  agent already advertises — the same update a browser client receives. No
  extension was needed for it, and the web client was ported from beam's model
  rather than designed fresh.
- **The best shape for using contenox is not a terminal.** Building and living
  with beam is what established that the browser client over the relay is the
  shape people reach for. That finding is the reason this page exists.

## Where it went

Extracted to its own repository: **[contenox/beam](https://github.com/contenox/beam)**.

It stays a terminal client for the Agent Client Protocol, able to connect to
any ACP agent, and keeps the contenox dialect — the extension methods and
config-option conventions — so every feature it had against contenox still
works. Shipped by the installer alongside contenox, with an opt-out.

## Why extracted rather than deleted

A client that only ever talks to one server proves nothing about the server. A
terminal client in its own repository, speaking the published protocol,
connecting to contenox and to other agents, is the strongest available evidence
that the protocol is really the seam and that nothing is held back behind it.

The maintenance argument points the same way: 14,397 production lines and 203
golden files are a real cost to carry inside a server, and none of it is the
server.
