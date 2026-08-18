---
title: "beam: the terminal client"
description: The lab record of the first beam — a full-screen terminal client for agent sessions, chat, plan, shell and file edits in one scrollback, approvals answered inline — and the 2026-08 addendum withdrawing its closing verdict.
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

## Addendum, 2026-08-18: the conclusion was drawn one step too far

This record closed by retiring the client, and the reason given at the time was
that the best shape for using contenox is not a terminal. That was an overreach.
It read a defect in *where the boundary sat* as a verdict on *what the surface
was*, and the two were never the same claim.

The finding that held is the second one above: **a surface must not own policy.**
Beam reached past the protocol, so a governance boundary lived inside a client,
and a boundary that only holds while one particular process is running is not a
boundary. Retiring the client did make that specific instance go away. It did
not fix anything — the next client would have grown the same reach, because
nothing underneath it was answering the question.

What answered it was the durable ask. A gated call now becomes a row and a
checkpoint before any surface renders anything: the verdict is recorded once, by
compare-and-swap, and the run resumes from the checkpoint exactly once, whether
the answer came from a terminal, an editor, or a phone three days later. The
policy is enforced under every client rather than by the one in front of you.
With the boundary moved, the reason to have no terminal client expired, and what
was left was the part of beam that was always good: a person at their own
keyboard, in their own scrollback, with the gate visible in front of them.

So beam is back in-tree, as the first-party client and the front door —
`contenox beam`, and bare `contenox` on a terminal. It is not this client
restored; it is a client written against a contract that had stopped moving,
which is what [Beam Desktop](/docs/rnd/beam-desktop/) said the next one would
need. The lab record above stands as written. Only its closing verdict is
withdrawn.
