---
title: "beam: the terminal client"
description: A full-screen terminal client for agent sessions — chat, plan, shell and file edits in one scrollback, with approvals answered inline.
---

# beam: the terminal client

`beam` was a full-screen terminal client for agent sessions: chat, plan, shell
and file edits in one scrollback, an approval card that blocked a gated tool
call inline, a command palette with argument completion, `@` file addressing,
and a status bar carrying the live model, session and context pressure.

![beam running a real session: the brand header with model and session id, the
`/ @ ! ?` affordances, an operator prompt asking what is needed to take Stripe
checkout live, and the agent's `local_fs.read_file` calls resolving inline as
it reads the setup doc and the deployment manifest before answering](/lab-beam-session.png)

It was the instrument that exercised every seam of the system from a client's
side.

## What it settled

- **The seam is the protocol, not the surface.** Almost none of beam knew which
  agent it was talking to. What it had been all along, without being asked to
  be one, was a client for the Agent Client Protocol.
- **A surface must not own policy.** Where beam did reach past the protocol,
  that was a governance boundary having quietly come to live inside a client. A
  boundary that only holds while one particular surface is running is not a
  boundary.
- **Argument completion belongs to the protocol.** Beam's command palette
  filled its value domains from what the agent already advertised — the same
  thing a browser client receives. Nothing had to be added to the protocol for
  it, and the web client was built from beam's model rather than designed
  fresh.
