---
title: The relay protocol
description: The wire contract between a contenox runtime and a relay — framing, the envelope, the handshake and its signature, resumption, and what a relay must never do. Enough to run your own.
order: 4
---

# The relay protocol

A contenox runtime reaches a browser or a phone through a **relay**: a
rendezvous point both ends connect to, because the machine running contenox
usually has no address anyone can dial.

You do not have to use ours. The protocol is in the open-source tree —
[`librelay`](https://github.com/contenox/contenox/tree/main/librelay) sits at
the module root rather than under `internal/` precisely so a relay written by
someone else can import it. This page is the contract; the package is the
reference implementation of every part of it that both ends must agree on.

Point a runtime at your own relay with two settings: the endpoint to dial and
the relay's public key.

For a hosted relay, or one run inside your own boundary, mail
**hello@contenox.com**.

## Shape

```
  runtime ──dials out──▶  relay  ◀──WebSocket──  browser
   (agent)                 │                      (client)
                           └── routes on (instance, session), never parses
```

Three properties hold for every implementation:

- **The runtime dials out and holds the connection.** Nothing on the runtime
  side listens. That is the entire reason a relay exists.
- **The relay never parses cargo.** `Frame.Payload` is raw JSON so an
  intermediary can route without decoding it. A relay that unmarshals ACP has
  taken a dependency this design does not have.
- **The relay is not a delivery guarantee.** It is a pipe with a cursor; see
  [Resumption](#resumption).

## Framing

NDJSON — one JSON frame per line, the same framing ACP uses. Because the
payload is already a JSON value, a connector splices an ACP line into a frame
with a byte copy and no re-encode, and neither end links a second parser.

## The envelope

```json
{ "type": "acp.message", "instance": "…", "session": "…",
  "id": "…", "re": "…", "seq": 12, "payload": { } }
```

| Field | Meaning |
|---|---|
| `type` | discriminates the frame. Control types are prefixed `relay.` (`ControlPrefix`); everything else is routed opaquely |
| `instance` | the runtime this frame concerns. Empty only before identification |
| `session` | the stream within the instance. **Routed on without being interpreted** |
| `id` | marks the frame a request. Its presence — not the type — obliges exactly one answer |
| `re` | the `id` being answered; marks a response |
| `seq` | the producer's per-`(instance, session)` cursor, monotonic and gap-free. Zero means unsequenced |
| `payload` | raw JSON, never re-encoded by an intermediary |

The types on the wire: control is `relay.hello`, `relay.welcome`,
`relay.heartbeat`; cargo is `acp.message`, `acp.detach`, and `session.resume` /
`session.resumed`.

Routing is `(instance, session)` and nothing else. A relay serving several
attached clients on one instance gives each a fresh attachment id and puts it
in `session`; the connector must echo it back unchanged on every frame for that
attachment. A frame arriving with no `session`, or one no attachment holds, is
dropped.

## Authentication

Two directions, two mechanisms, deliberately asymmetric.

**Instance proves itself to the relay** with a bearer credential on the
transport's upgrade request. It never enters a frame, so it never enters
`librelay`.

**The relay proves itself to the instance** with an Ed25519 signature. The
connector generates a fresh nonce per connection and the relay signs it:

```
SigningInput = SigningDomain ‖ 0x00
             ‖ base64rawurl(nonce) ‖ 0x00
             ‖ decimal(negotiatedVersion) ‖ 0x00
             ‖ instance
```

Use `librelay.SigningInput`, `SignWelcome` and `VerifyWelcome` rather than
rebuilding this. A signature format defined twice is defined differently — the
reason the function is exported at all.

Constraints the verifier enforces: nonce is 32 bytes (`NonceSize`) and never
larger than 64 (`MaxNonceBytes`), which bounds the work an unauthenticated peer
can ask for with one hello; a NUL byte anywhere in `instance` is refused,
because it would move the separators. **The version signed is the one the relay
selected**, not the one the connector offered.

This is application-layer auth and deliberately not TLS pinning: a relay's
certificate comes from an ACME CA and rotates every ~90 days, so a binary
pinning the leaf would break itself in the field on machines nobody can reach.
The signing key is long-lived and travels with the pairing.

## The handshake

```
connector                                   relay
    │                                         │
    │  dial (TLS) + bearer credential ───────▶│
    │                                         │
    │  hello { protocol_version,              │
    │          instance, agent, nonce } ─────▶│
    │                                         │
    │◀──────── welcome { protocol_version,    │
    │                    relay, sig,          │
    │                    retry_after }        │
    │                                         │
    │  VerifyWelcome(key, nonce,              │
    │                welcome.version,         │
    │                instance, sig)           │
    │                                         │
    │◀────────── frames flow both ways ──────▶│
```

`Welcome.ProtocolVersion` is `min(peer, self)`. **A connector closes if it is
lower than it can speak.** `RetryAfterSeconds` hints at a redial delay and a
connector clamps it to its own bounds; zero means the connector's own backoff
applies.

A relay that cannot prove itself is a fatal error, not a transient one — it
will not start being able to on the next dial.

## Heartbeats

`TypeHeartbeat` probes liveness. The connector's defaults are a **15s interval
and a 10s timeout**; a peer that does not answer ends the connection. A relay
should answer promptly and may probe on its own cadence.

## Resumption

A dropped connection costs latency, never content.

`Frame.Seq` is the producer's per-session cursor. On reconnect the **receiver**
sends `Resume{after_seq}` with the last value it saw, and the producer
continues after it. `Resumed{from_seq}` announces what follows; `from_seq`
exceeds `after_seq + 1` only when retention evicted frames in between. Zero
asks for the whole retained stream.

That is SSE's model — one cursor per connection rather than an acknowledgement
per message — and it is why nothing here carries delivery guarantees.

Resumption is cargo, not control: `session.resume` routes end to end and a
relay treats it as any other payload. It travels one way only — a producer
replays, a consumer does not.

## Control versus cargo

One `type` field carries both, so a receiver makes exactly one decision per
frame: the `relay.` prefix means "for me", anything else means "route it". Two
fields could disagree, and every disagreement is a case somebody has to invent
behaviour for.

Unknown is never fatal, in both directions:

- Unknown **fields** are ignored — the envelope evolves by addition only, and a
  field may never change meaning.
- An unknown **non-control** type is opaque and gets routed on.
- An unknown **control** type is answered with `Unsupported` when it is a
  request and dropped when it is not. A request always gets exactly one reply,
  so a newer peer talking to an older one fails fast instead of blocking.

The "ignored" rule does not hold for a relay. An endpoint decodes a frame and
acts on it; a relay decodes and re-encodes to forward, so an unknown field is
destroyed rather than ignored.

**Build a relay against a `librelay` at least as new as the endpoints it
serves.**

## What a relay must never do

- **Parse ACP.** Route on the envelope. The payload is not yours.
- **Re-encode with an older envelope.** See above.
- **Invent a second implementation of the signature.** Import `librelay`.
- **Treat `session` as meaningful.** It is an opaque routing key.

## Building one

```go
import "github.com/contenox/contenox/librelay"
```

The package gives you the envelope and codec (`Frame`, `NewReader`,
`NewWriter`), every control payload (`Hello`, `Welcome`, `Resume`, `Resumed`,
`Ack`, `Heartbeat`, `Error`, `ChainTrigger`), both halves of the signature
(`SignWelcome`, `VerifyWelcome`, `SigningInput`, `NewNonce`), key handling
(`ParsePublicKey`, `FormatPublicKey`) and trace ids.

What you supply is the server: accept the upgrade, authenticate the bearer
credential, hold connections per instance, and route frames between an instance
and the clients attached to it.

Outside Go, this page is the specification — but note the re-encode rule above
before writing a second envelope implementation.

## Related

- [Task chains](/docs/specification/) — what runs on the other end
- [Pairing](/docs/guide/pairing/) — how a machine and a relay are introduced
