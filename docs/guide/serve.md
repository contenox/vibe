---
title: "contenox serve: the standing host"
description: The organization's shape — a standing host serving one workspace fixed at launch, with no filesystem and no terminal tools, driven by connectors and event triggers through the relay.
order: 13
---

# contenox serve: the standing host

The three shapes contenox runs in are partitioned by one question: who is
accountable for the machine.

In [`contenox beam`](/docs/reference/contenox-cli/#contenox-beam) that is you,
at the keyboard, on a device you own. In `contenox run` it is whoever wrote the
script. `contenox serve` is the third answer: **an organization is accountable**
for a host that nobody is sitting at.

```bash
contenox serve
```

That difference is not administrative. It decides what the host is allowed to
be, and the rest of this page follows from it.

## One workspace, fixed at launch

A host serves exactly **one** workspace, chosen when you start it and immutable
until you stop it:

```bash
contenox serve              # your home directory
contenox serve .            # the directory you are standing in
contenox serve ~/src/api    # one project
```

With no path the host serves your **home directory**, not the directory you
started it from. A host outlives the shell that launched it and is reached from
a device that knows nothing about where that shell was standing, so scoping it
to the launch directory would make its scope depend on an invisible detail.
`contenox serve .` is how you ask for the narrow scope, and it says so where the
next person can read it.

There is no workspace selection anywhere in the system — no picker in beam, in
an editor, or in the app. A client does not choose where a session runs; it
discovers instances and the sessions they are already holding, and the workspace
is whatever the instance was launched with. Serving a second workspace means
starting a second instance, which is also how you say, in the process list,
that it is a different thing.

## No filesystem. No terminal. Ever.

A host has **no `local_fs` and no `local_shell`**, under any policy, on any
run. This is not a strict preset — those tools are absent from the shape.

They are absent because the client that would carry them is absent. `local_fs`
and `local_shell` are forwarded to a connected client's `fs/*` and `terminal/*`
capabilities: beam performs them on your machine, an editor performs them in the
project you have open. A standing host has no such client, and a host that
performed them itself would be an agent with a shell on a shared box that
nobody is watching.

So every capability a host has is an **MCP server** you attached, or an HTTP
service wrapped from an OpenAPI spec:

```bash
contenox mcp add notion https://mcp.notion.com/mcp --auth-type oauth
contenox tools add erp_billing --url https://erp.internal.example.com --spec ./billing-subset.yaml
```

That constraint is what makes the host reviewable. The set of things it can
touch is a list you wrote and can read back, each entry a named service with its
own credentials, rather than "whatever is on that disk". Everything else is
unchanged: the same declarations, the same envelope, the same durable asks.

- [MCP integration](/docs/integrations/tools/mcp/) — attaching a server
- [Remote tools](/docs/integrations/tools/remote/) — an OpenAPI subset as a tool

## What drives it

Nobody types into a host. Work reaches it two ways.

**Through the relay.** A paired machine holds a connection open, and the
[contenox app](https://app.contenox.com) attaches over it — reading a
transcript, answering an ask, starting a session against the workspace the host
was launched with. See [Pairing a machine with a relay](/docs/guide/pairing/).

**Through triggers.** Internal domain events land in a durable log, and
operator-authored `trigger-*.json` files fire chains from them — a mission
report, a status change, an ask that has been waiting too long. Opt-in and beta;
see [Events & triggers](/docs/guide/events/).

Either way the envelope is what bounds it. A host is the shape where that
matters most, because there is no human in the loop by default — only the ones
your policy stops for.

## What it prints

The host checks its own setup before serving and then reports what the process
actually is:

```
  Setup        ready
  Workspace    /home/you
  ID           8d7db1ed-329d-4300-83ac-ad325c2e8d75
  Model        vertex-google/gemini-2.5-pro
  Relay        attached to https://relay.contenox.com
  Instance     ca2e8376-99ab-4669-9ca4-b32e5605bb4d
  App          https://app.contenox.com
  Logs         ~/.contenox/logs/serve-2026-08-15.log
               new part at 50MB · keep 4 files · 7 days

  Running. Press Ctrl-C to stop.
```

Every line is a fact about this process rather than a claim about what should
be true. The setup row runs the same readiness check as
[`contenox doctor`](/docs/reference/contenox-cli/#contenox-doctor); a host with
no model configured says so and still serves, because being reachable is worth
something even when nothing can run yet.

An unpaired host prints the steps to pair it and keeps running — it is simply
reachable on that machine only.

The instance token is never printed.

## When one host is not enough

State lives in a local SQLite file by default, which is the right answer for one
host. Three environment variables move the store, the message bus and the
key-value cache onto infrastructure you already run — Postgres, NATS and Valkey
— so several hosts share one control plane. Unset, they are absent and the file
is all there is. See
[External backends for state](/docs/reference/config/#external-backends-for-state-opt-in).

## Logs

A host is meant to run for days, so its structured logs go to files rather than
over the top of the status screen. They live in `<data-dir>/logs` — override
with `--log-dir` — and are organised by date:

```
~/.contenox/logs/
  serve-2026-08-14.log
  serve-2026-08-15.log
  serve-2026-08-15.2.log
```

A day that outgrows its size bound continues in a numbered part, so "what did
this host do on Tuesday?" stays a question about filenames. Parts restart at
one each day, and restarting a host continues the current part rather than
opening a file per launch.

Retention is bounded in both directions, because either bound alone leaves a
gap: a file count never retires a quiet host's logs, and an age limit never
bounds a busy one's disk.

```bash
contenox config set log-max-size 50MB      # new part at this size
contenox config set log-max-files 4        # kept across every date and part
contenox config set log-max-age-days 7     # retired by date
```

Sizes take an optional unit (`10MB`, `512KB`, `1GB`, or plain bytes). Either
retention key accepts `0` for "no limit". The defaults — 10MB parts, 14 files,
14 days — bound an unconfigured host without being asked. Full descriptions are
in the [configuration keys](/docs/reference/config/#set-persistent-defaults) table.

Two things the host will not do: delete a file it does not own (only its own
`serve-<date>.log` names are ever candidates, so a log directory shared with
something else is safe), and delete the file it is currently writing, however
tight the bound.

Host logs are separate from `telemetry.log`, so turning telemetry on or off
never changes whether a host has somewhere to write its diagnostics.

## Stopping it

`Ctrl-C`. The host stops dialling, in-flight work is torn down, and the machine
becomes unreachable from the app until a host or a session runs again. Stopping
a host does not unpair it — the machine stays attached to the relay and is
reachable again the moment something starts. To detach the machine entirely,
use `contenox unpair`.

A run that was waiting on an ask when you stopped the host is not lost. The ask
is a durable row, and the run checkpoints beside it on the way out; both outlive
the process, so answering the ask later resumes the run — in the next host to
start, or in the terminal that answers it.

## See also

- [CLI reference: `contenox serve`](/docs/reference/contenox-cli/#contenox-serve-path)
- [Pairing a machine with a relay](pairing.md) — attaching the machine in the first place
- [Missions](missions.md) — the unattended work order a host mostly runs
- [Sovereignty](sovereignty.md) — what stays on your machine, and why
