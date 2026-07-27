---
title: Least-privilege shell environment
description: The scrub-and-inject design for the shells contenox runs in its own process — give an agent exactly the environment its task needs, not your whole .env. The policy surface exists; no spawn site applies it today.
---

# Least-privilege shell environment

> [!WARNING]
> **Not in effect today.** Neither the scrub nor the injection below is applied to
> any shell contenox spawns. The wiring lived in `contenox serve`, which was cut;
> the surviving surfaces (`beam`, `acp`, `chat`, `run`) never took it up. The
> policy, the `SANDBOX_*` variables, `contenox shell-env` and the `contenox
> sandbox env` preview all still exist — they just govern nothing, so a shell an
> agent drives inherits the runtime's environment whole. The one environment that
> IS scrubbed is a foreign agent's, by the
> [agent sandbox](/docs/guide/agent-sandbox/). Treat this page as the design of
> the surface, not a control you can rely on.

An agent that can run a shell can read its environment — and by default that environment is the contenox process's own, with every variable it was started with. The usual way an agent gets a value it needs, say a `DATABASE_URL`, is to read your `.env`; but reading `.env` for one variable hands it the `STRIPE_SECRET_KEY` two lines below, too.

This surface is built to invert that: each spawned shell — the `local_shell` tool, the `shell_session` / `!` PTY, the interactive terminal — gets exactly the environment a task needs, in two moves:

- **scrub** — strip the runtime's own credentials out of the shell, so there is nothing to leak;
- **inject** — add back only the variables you choose, with the values you set.

The agent gets its `DATABASE_URL`, does the work, and your other secrets are never in the room. That is the environment slice of least privilege: deny by default, grant what the job needs. It is configuration today with no enforcement behind it — see the warning above.

## How it works

Scrubbing is **configured on by default** for agent-reachable shells — `deny-secrets` is the policy `contenox sandbox env` reports, and no spawn site applies it today (see the warning above). The design is two layers:

1. The **scrub** filters the contenox process's environment down to a policy you choose. The default, `deny-secrets`, keeps the toolchain's variables but drops the control plane and the common credential shapes.
2. The **injection** overlays your own variables on top, so an injected value always wins — and applies even when the scrub is `off`.

The scrub policy is set per surface:

| Surface | Who drives it | Scrub variable | Default |
|---|---|---|---|
| `local_shell` and the `!` / `shell_session` PTY | the agent | `SANDBOX_SHELL_SCRUB` | `deny-secrets` |
| the interactive terminal panel | the operator, typing directly | `SANDBOX_TERMINAL_SCRUB` | `off` |

Agent-reachable shells scrub by default because the agent is untrusted. The terminal panel is the operator's own shell, so it defaults to `off`; set it to `deny-secrets` or `strict` when you want the same guarantees there.

## Scrub: deny by default

Each scrub variable takes one of three modes:

| Mode | What passes through |
|---|---|
| `off` | The full environment — no scrubbing. |
| `deny-secrets` | Everything **except** the control plane (`CONTENOX_*`), the common credential shapes, and anything in `SANDBOX_ENV_DENY`. |
| `strict` | **Only** a safe base set plus anything in `SANDBOX_ENV_ALLOW`; everything else is absent. |

**`deny-secrets`** is the lowest-breakage posture and the default for agent shells: a toolchain keeps the environment it expects, while these are stripped —

```
CONTENOX_*     *_TOKEN     *_KEY     *_SECRET
*_PASSWORD     *_PASSWD    *_CREDENTIALS
```

**`strict`** hands the shell only the safe base set —

```
PATH   TERM   COLORTERM   TZ   LANG   LANGUAGE   LC_*
TMPDIR   USER   LOGNAME   SHELL
```

— plus whatever you name in `SANDBOX_ENV_ALLOW`. In `strict` the only denies are the control plane and your explicit `SANDBOX_ENV_DENY`, so you can even re-permit a specific inherited credential by naming it. (In `deny-secrets` the credential-shape denies always win, so to pass a specific inherited secret switch to `strict` — or inject the value with `shell-env`, below.)

`SANDBOX_ENV_ALLOW` and `SANDBOX_ENV_DENY` are comma- or whitespace-separated lists of names or globs. A glob is a single leading or trailing `*`: `LC_*` (prefix), `*_TOKEN` (suffix); matching is case-sensitive.

```bash
# Agent shells scrubbed of secrets (the default); lock down the operator terminal too:
SANDBOX_TERMINAL_SCRUB=deny-secrets contenox beam

# Hand agent shells only a hand-picked environment:
SANDBOX_SHELL_SCRUB=strict SANDBOX_ENV_ALLOW="GOCACHE,CARGO_HOME,HTTP_PROXY" contenox beam
```

> [!NOTE]
> Whenever a scrub is active, `CONTENOX_*` — the control plane's own variables — is **always** dropped, and `HOME` is left as the operator's real home. In `off` mode nothing is scrubbed.

## Inject: grant what the task needs

`SANDBOX_ENV_ALLOW` *passes through* a variable that is already in the process's environment. To give a shell a variable that is **not** in the environment — or to set one to a value you choose — inject it directly. Injected variables are global (every spawned shell), stored as plain configuration, and read live, so an edit applies to the next shell without a restart.

```bash
contenox shell-env set DATABASE_URL=postgres://localhost/app HTTP_PROXY=http://proxy:3128
contenox shell-env list
contenox shell-env unset HTTP_PROXY
```

Injected values are layered on top of the scrub, so they always win and apply even when the scrub mode is `off`. They are plain config, **not** a place for secrets.

> [!NOTE]
> `SANDBOX_ENV_ALLOW` and `shell-env` are different tools. `SANDBOX_ENV_ALLOW` *passes through* a variable the process already has; `shell-env` *sets* a variable to a value you choose, whether or not the process has it.

## Verify before you trust it

Preview exactly which variable names a shell would inherit from the contenox process's environment under the current scrub —

```bash
contenox sandbox env            # the agent-shell policy
contenox sandbox env --terminal # the interactive-terminal policy
```

— and list what you inject on top:

```bash
contenox shell-env list
```

`sandbox env` is a dry run of the **policy** against the live environment (names only, values withheld). It shows what the scrub would strip — not what a spawned shell actually receives, which today is the unfiltered environment.

## How it relates to the agent sandbox

This is the **environment** slice of a larger least-privilege architecture: an agent should reach only what its task needs, and nothing else. The [agent sandbox](/docs/guide/agent-sandbox/) is the rest of "the wall" — the filesystem and exec surface of a spawned foreign agent made absent by construction, so it cannot read your `.env` off disk any more than from the environment. That wall is built and fail-closed, and it scrubs the environment of the agents it confines — though nothing reaches it on a stock install yet (see its status note). This page's scrub is the same idea applied to the shells contenox runs *in its own process*, where the wall does not reach — and it is the piece that is not wired.
