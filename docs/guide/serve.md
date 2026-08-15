---
title: "Reaching a machine from the app"
description: Running contenox as a host with contenox serve — what the status screen reports, how the workspace root is chosen, and how its logs are organised and bounded.
---

# Reaching a machine from the app

A paired machine is reachable only while something on it is running. An editor
session counts, but tying a machine's availability to an editor is a poor
bargain: close the editor and the machine goes quiet, and a headless box has no
editor to open in the first place.

`contenox serve` is that something — a host. It builds the same runtime an
editor session does, holds the relay connection open, and stays up until you
stop it.

```bash
contenox serve
```

`contenox beam` is an alias for the same host.

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
reachable on that machine only. See
[Pairing a machine with a relay](/docs/guide/pairing/).

The instance token is never printed.

## Which directory it serves

The optional path is the default workspace root for sessions the app opens, and
the first entry in the [workspace roots](/docs/reference/contenox-cli/#workspace-roots)
allowlist that bounds every attachment:

```bash
contenox serve              # your home directory
contenox serve .            # the current directory
contenox serve ~/src/api    # one workspace
```

With no path the host serves your **home directory**, not the directory you
started it from. A host outlives the shell that launched it and is reached from
a device that knows nothing about where that shell was standing, so scoping it
to the launch directory would make what the app can open depend on an invisible
detail. `contenox serve .` is how you ask for the narrow scope, and it says so
where the next person can read it.

Additional roots are granted the usual way, with `contenox workspace add` or
`--workspace-root`; the served path is simply the default among them.

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
becomes unreachable from the app until a host or an editor session runs again.
Stopping a host does not unpair it — the machine stays attached to the relay and
is reachable again the moment something starts. To detach the machine entirely,
use `contenox unpair`.

## See also

- [Pairing a machine with a relay](pairing.md) — attaching the machine in the first place
- [CLI reference: `contenox serve`](/docs/reference/contenox-cli/#contenox-serve-path--contenox-beam-path)
- [Sovereignty](sovereignty.md) — what stays on your machine, and why
