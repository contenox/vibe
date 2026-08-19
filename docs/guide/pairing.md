---
title: "Pairing a machine with a relay"
description: How pairing attaches a machine to a relay so it can be reached from a phone — what is sent, what is stored, and how to undo it.
order: 14
---

# Pairing a machine with a relay

An agent you can walk away from is only half a promise if the only place you can
answer it is the desk it is running on. A **relay** closes that: your machine
dials out and holds a connection, and you supervise it from somewhere else.

Pairing is how a machine gets permission to do that. It is opt-in, it is one
command, and until you run it **nothing about your machine leaves it**.

## The flow

You need the contenox app, signed in. It lives at
[app.contenox.com](https://app.contenox.com). An account is free — for you
and up to three teammates, one machine each.

1. In the app, tap **Pair device**. It shows a six-character key and a
   countdown.
2. On the machine, redeem the key:

   ```bash
   contenox pair K7M-3PQ
   ```

That is the whole flow. No browser opens, on either device. That matters more
than it sounds: half the machines worth pairing are headless boxes, containers
and WSL, where there is no browser to open and an attempt to launch one is a
failure rather than a convenience. A typed key works the same everywhere.

The key is short-lived and can be redeemed exactly once. If it expires while you
are walking to the machine, mint another — they cost nothing.

### From a session instead

If you are already in a session — `contenox beam`, or an editor — `/pair
K7M-3PQ` does the same thing without leaving it. A pairing describes the **machine**, not the
process that redeemed the key or the directory it was standing in, so both
entry points write the same credential and every later process finds it.

## Being reachable

Pairing attaches the machine. Something then has to be running for the app to
attach *to*. Any session will do — a `contenox beam` at your desk, an editor
session — and for a machine that should stay reachable with nobody at it, a
host:

```bash
contenox serve
```

That is the standing host: one workspace, fixed at launch, no editor and no
person in front of it. It prints what it is attached to and stays up until you
stop it. See [contenox serve: the standing host](/docs/guide/serve/).

`/link` typed into a session prints the direct link that opens that same session
in the app, so picking it up on a phone does not start with hunting through the
list. The app does not choose a directory to work in: it discovers the instances
this machine is running and the sessions they already hold.

## Choosing a relay

By default a key is redeemed against the hosted relay, whose address and
identity key ship in the binary. Shipping an address contacts nothing: pairing
is the only call that reaches a relay, and it runs only when you type a key.

To point a machine at a different relay, set the environment:

```bash
export CONTENOX_RELAY_ENDPOINT=https://relay.example.internal
```

Or pass it inline:

```bash
contenox pair K7M-3PQ https://relay.example.internal
```

Self-hosting is the same mechanism, not a second one. Your relay hands out its
own public key at redemption, and the machine verifies that key from then on.
A self-hosted machine is pointed at your relay's own address everywhere it is
shown — the hosted service is not substituted back in.

## What is sent, and what is stored

When you redeem a key, your machine sends exactly two things: **the key**, and
**its hostname** — the name your fleet list will show. Nothing else. No files,
no paths, no environment, no session content, no model keys.

What comes back is written to `~/.contenox/relay.json`:

| Field | What it is |
| --- | --- |
| `endpoint` | The relay this pairing is for |
| `instance_token` | This machine's credential. Secret. |
| `instance_id` | This machine's identity at the relay |
| `account_id` | The account it now belongs to |
| `relay_public_key` | How this machine recognises that relay, forever after |

The credentials live in your home directory rather than beside a project,
because a pairing describes the **machine**, not the directory you happened to
be standing in.

Two properties are worth stating plainly, because they are the ones that would
matter if they were untrue:

- **The instance belongs to the account, not to you.** Whoever minted the key is
  recorded for audit, but the machine is the account's. That is what makes "a
  colleague answers the approval you are asleep for" a permission rather than a
  future rewrite.
- **The relay's public key is checked, not the TLS certificate.** Your machine
  refuses any relay that cannot sign with the key it was given at pairing, which
  is what defends against a rogue certificate authority or corporate TLS
  interception. It is deliberately *not* certificate pinning — that certificate
  rotates every ninety days, and a binary pinned to it would break itself in the
  field on machines nobody can reach.

## Turning it off

```bash
contenox unpair          # or /unpair from inside a session
```

This is local: it deletes the credential file, and this machine stops dialling.
It does **not** revoke anything. Revoking an instance is done in the app, and a
revoked machine is refused at its next dial whether or not it still holds the
file — so if a machine is lost or stolen, revoke it there rather than hoping to
reach it.

You can also just delete `~/.contenox/relay.json`. There is nothing else to
clean up, and no daemon to stop.

To check what a machine is attached to without changing anything:

```bash
contenox pair            # or /pair from inside a session
```

It prints the relay, the instance and the account. It never prints the token.
`contenox serve` shows the same attachment on its status screen.

## Not pairing at all

The relay is optional and always was. A contenox install that is never paired
behaves exactly as it did before: local models or your own provider keys, local
SQLite, no account, nothing outbound. Every feature that does not involve
reaching your machine from elsewhere works the same.

## See also

- [contenox serve: the standing host](serve.md) — the host the app attaches to when nobody is at the machine
- [Human gates and envelopes](hitl.md) — what holds a run and waits for you
- [Sovereignty](sovereignty.md) — what stays on your machine, and why
