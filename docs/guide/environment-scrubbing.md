---
title: Least-privilege shell environment
description: The scrub-and-inject design for the shells contenox runs in its own process — give an agent exactly the environment its task needs, not your whole .env.
---

# Least-privilege shell environment

An agent that can run a shell can read its environment. By default that environment is the contenox process's own, with every variable it was started with. The usual way an agent gets a value it needs — say a `DATABASE_URL` — is to read your `.env`, but reading `.env` for one variable also hands it the `STRIPE_SECRET_KEY` two lines below.

This surface gives each spawned shell — the `local_shell` tool, the `shell_session` / `!` PTY, and an ACP session's own shell — exactly the environment a task needs, in two steps:

- **scrub** — strip the runtime's own credentials out of the shell, so there is nothing to leak
- **inject** — add back only the variables you choose, with the values you set

This is the environment slice of least privilege: deny by default, grant what the job needs. It is live today across every agent-reachable shell this CLI spawns — `contenox chat` / `contenox run` (`local_shell`), `contenox acp` / `contenox acpx` (the ACP session's shell), and `contenox new` (`local_shell` and the `!` PTY).

## How it works

Scrubbing is **on by default** for agent-reachable shells: `deny-secrets`, applied through one shared composition (`resolvedSandboxEnv`) that every spawn site calls, so the policy can never drift between surfaces. The design has two layers:

1. The **scrub** filters the contenox process's environment down to a policy you choose. The default, `deny-secrets`, keeps the toolchain's variables but drops the control plane and the common credential shapes.
2. The **injection** overlays your own variables on top, so an injected value always wins — and applies even when the scrub is `off`.

The scrub policy is set per surface:

| Surface | Who drives it | Scrub variable | Default |
|---|---|---|---|
| `local_shell` and the `!` / `shell_session` PTY | the agent | `SANDBOX_SHELL_SCRUB` | `deny-secrets` |
| the interactive terminal panel | the operator, typing directly | `SANDBOX_TERMINAL_SCRUB` | `off` |

Agent-reachable shells scrub by default because the agent is untrusted. The terminal panel is the operator's own shell, so it defaults to `off`; set it to `deny-secrets` or `strict` for the same guarantees there.

> [!NOTE]
> `SANDBOX_TERMINAL_SCRUB` and `contenox sandbox env --terminal` are real, working config and preview today — they just have no consumer yet: contenox does not currently spawn a distinct operator-facing terminal panel (as opposed to the agent-reachable `local_shell` / `!` PTY covered by `SANDBOX_SHELL_SCRUB` above). Configure it now for when that surface lands, or ignore it — it governs nothing yet.

## Scrub: deny by default

Each scrub variable takes one of three modes:

| Mode | What passes through |
|---|---|
| `off` | The full environment — no scrubbing. |
| `deny-secrets` | Everything **except** the control plane (`CONTENOX_*`), the common credential shapes, and anything in `SANDBOX_ENV_DENY`. |
| `strict` | **Only** a safe base set plus anything in `SANDBOX_ENV_ALLOW`; everything else is absent. |

`deny-secrets` is the lowest-breakage posture and the default for agent shells: a toolchain keeps the environment it expects, while these are stripped —

```
CONTENOX_*     *_TOKEN     *_KEY     *_SECRET
*_PASSWORD     *_PASSWD    *_CREDENTIALS
```

`strict` hands the shell only the safe base set —

```
PATH   TERM   COLORTERM   TZ   LANG   LANGUAGE   LC_*
TMPDIR   USER   LOGNAME   SHELL
```

— plus whatever you name in `SANDBOX_ENV_ALLOW`. In `strict` the only denies are the control plane and your explicit `SANDBOX_ENV_DENY`, so you can re-permit a specific inherited credential by naming it. In `deny-secrets` the credential-shape denies always win; to pass a specific inherited secret, switch to `strict`, or inject the value with `shell-env` instead (below).

`SANDBOX_ENV_ALLOW` and `SANDBOX_ENV_DENY` are comma- or whitespace-separated lists of names or globs. A glob is a single leading or trailing `*`: `LC_*` (prefix), `*_TOKEN` (suffix). Matching is case-sensitive.

```bash
# Agent shells scrubbed of secrets (the default); lock down the operator terminal too:
SANDBOX_TERMINAL_SCRUB=deny-secrets contenox new

# Hand agent shells only a hand-picked environment:
SANDBOX_SHELL_SCRUB=strict SANDBOX_ENV_ALLOW="GOCACHE,CARGO_HOME,HTTP_PROXY" contenox new
```

> [!NOTE]
> Whenever a scrub is active, `CONTENOX_*` — the control plane's own variables — is always dropped. In `off` mode nothing is scrubbed. `HOME` and `PATH` are not forced to anything special here: an agent shell runs in the operator's own home and inherited toolchain, scrubbed of credentials rather than reshaped into a different filesystem identity — that reshaping is what the separate [agent sandbox](/docs/guide/agent-sandbox/) does for a foreign agent process.

## Inject: grant what the task needs

`SANDBOX_ENV_ALLOW` passes through a variable that is already in the process's environment. To give a shell a variable that is **not** in the environment, or to set one to a value you choose, inject it directly. Injected variables are global (every spawned shell), stored as plain configuration, and read live, so an edit applies to the next shell without a restart.

```bash
contenox shell-env set DATABASE_URL=postgres://localhost/app HTTP_PROXY=http://proxy:3128
contenox shell-env list
contenox shell-env unset HTTP_PROXY
```

Injected values are layered on top of the scrub, so they always win and apply even when the scrub mode is `off`. They are plain config, not a place for secrets.

> [!NOTE]
> `SANDBOX_ENV_ALLOW` and `shell-env` are different tools. `SANDBOX_ENV_ALLOW` passes through a variable the process already has; `shell-env` sets a variable to a value you choose, whether or not the process has it.

## Verify before you trust it

Preview which variable names a shell would inherit from the contenox process's environment under the current scrub:

```bash
contenox sandbox env            # the agent-shell policy
contenox sandbox env --terminal # the interactive-terminal policy
```

List what you inject on top:

```bash
contenox shell-env list
```

`sandbox env` runs the exact same composition (`resolvedSandboxEnv`) every agent-reachable shell is built with, evaluated against this process's own environment, so the preview can never drift from what a spawned shell actually receives. Values are always withheld — only names are printed — so the output is safe to share.

## How it relates to the agent sandbox

This is the environment slice of a larger least-privilege architecture: an agent should reach only what its task needs, and nothing else. The [agent sandbox](/docs/guide/agent-sandbox/) is the rest of the wall — the filesystem and exec surface of a spawned foreign agent, made absent by construction, so it cannot read your `.env` off disk any more than from the environment. This page's scrub is the same idea applied to the shells contenox runs in its own process (`local_shell`, the `!` PTY, an ACP session's shell) — a different surface from the agent sandbox's foreign-process wall, and one that is wired and applying today.
