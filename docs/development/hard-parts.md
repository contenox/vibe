---
title: The hard parts
description: Things that only show up once an agent harness is real — cache prefixes, rule ordering, silent field loss, environments a shell can read. What each one costs and what contenox does about it.
order: 1
---

# The hard parts

An agent loop is a weekend. Most of what follows is not in the loop.

These are traps this codebase hit and now handles. They are written down whether
or not you use contenox — if you are building your own harness, several of them
will find you.

## A shell inherits your credentials

An agent that can run a shell can read the environment it was started with. The
usual advice — put secrets in `.env` — does not help: reading `.env` for
`DATABASE_URL` also hands the agent the `STRIPE_SECRET_KEY` two lines below.

Spawned shells get the runtime's environment scrubbed and only what you inject
added back. It is not a kernel boundary and the page says so: on Linux a shell
that can read files can still reach `/proc/<pid>/environ`.

→ [Least-privilege shell environment](/docs/guide/confinement/environment/)

## Rule order decides what a policy means

Policies are first-match-wins. A deny placed after a grant never fires, so a
policy can read exactly right and permit exactly the thing it names.

Every emitted policy puts the credential-path deny first, and the test asserts
its *position*, not just that it exists.

## Omitting a budget is not "no budget"

`token_limit` sizes the chat history *and* the per-call cap on tool results.
Leave it out and the budget is zero, so every tool call comes back
`tool_result_too_large` — no matter how small the result actually was.

A chain whose tools all report that error is missing one field.

## A prompt is a cache key

Providers cache on a stable prefix, and the system instruction is that prefix.
Two consequences that are easy to miss:

- Wall-clock macros degrade to day granularity inside the prefix, or the bytes
  change on every request and nothing caches.
- Anything rendered from a map — a tool manifest, for instance — is sorted, so
  the prompt does not depend on registry enumeration order.

A harness that re-renders a slightly different prompt each turn pays full price
every turn and never notices.

## Implicit context is a trap

An earlier version appended a tool manifest to the system prompt unless the
prompt already contained the substring `"tool"`. A prompt mentioning "don't use
a tool without checking" silently lost its manifest.

Nothing is appended to a system instruction now. `{{tools}}` and `{{host:os}}`
are macros a chain declares. What a task states is what the model receives.

## Signatures defined twice are defined differently

Both ends of the relay handshake must compute one byte string. That computation
lives in one place, exported, and neither side reimplements it:

```go
librelay.SigningInput(nonce, negotiatedVersion, instance)
```

Two implementations of the same signature will agree in your tests and disagree
in production, and the failure looks like an auth bug rather than a formatting
one.

## An intermediary destroys what it does not understand

Endpoints ignore unknown fields, which is how a wire format evolves. An
intermediary that decodes and *re-encodes* does not ignore them — it destroys
them, silently, because nothing on either side can see they went missing.

So a relay must be built against a protocol package at least as new as the
endpoints it serves. This is a different rule from the one endpoints follow, and
getting it wrong loses data with no error anywhere.

→ [The relay protocol](/docs/specification/relay-protocol/)

## Nested files are not found

Chain discovery reads one directory level and skips subdirectories. A chain
filed neatly into `chains/reviews/` is not "not working" — it is not there at
all, and nothing reports it.

Generated files live in a directory discovery is handed explicitly, rather than
one it happens to walk.

## A silent no-op reads as success

A session's working directory and its agent are fixed when the session starts.
A client asking to change either gets an error, not a quiet no-op, because a
no-op reads to the client as a switch that took.

## Liveness is a timestamp, not a process check

Presence records expire on a TTL and are renewed by heartbeat. Nothing asks the
operating system whether a PID is alive: a PID is reused, a process can be
wedged rather than dead, and a check that works on one platform is a
reimplementation on the next.

## Answering twice is a different answer

An approval is a durable record with one verdict. Answering an already-answered
ask is refused rather than accepted, and a resumed run continues exactly once —
because a re-delivered instruction is a second instruction, not a retry.

## A knob that does nothing is worse than no knob

`compute.maxTurns` accepts any number and only the value `1` has an effect. It
is documented that way rather than quietly honoured, because a bound that looks
enforced and is not is worse than one that was never offered.

Emitted policies map a declaration's turn cap onto `maxToolCalls` instead, and
only ever tighten the shipped ceiling.

---

None of these are exotic. They are the ordinary cost of the second week —
after the loop works, before anyone else depends on it.
