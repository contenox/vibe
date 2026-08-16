---
title: "Confining agents: the sandbox wall"
description: How the default filesystem/exec/environment fence and the opt-in network wall confine a foreign ACP agent's process.
---

# Confining agents: the sandbox wall

The wall confines a **foreign agent**: an external ACP agent — code contenox did not write — spawned as a subprocess and given a workspace and a set of tools. It governs the surface no tool gate can see: the code the agent runs *inside its own process* — its own shell, its own file access, whatever its toolchain pulls in (an `npm install` postinstall script, for example). An agent that only uses its tools never touches the wall.

> **Important:**
> This page does not cover contenox's own chains. A `contenox serve` session, an editor `acp`/`acpx` session, and mission units the fleet dispatches all run outside the wall by design — see [what the wall does not confine](#what-the-wall-does-not-confine-contenox-itself). Registering a foreign agent is also not exposed yet: `external_acp` agents are internal-only, so a stock install never reaches the wall. The mechanism below is built and tested; it is waiting on the registration path.

There are two layers: one is always on and needs no configuration, the other is opt-in.

## The default: a zero-privilege fence

Every confined agent gets this by default, with no configuration and no kernel privilege:

- **Filesystem** — the agent may *write* only inside its workspace (its cwd), and *read* only its workspace plus a short list of auth/config directories (`~/.claude`, `~/.codex`, `~/.config/goose`, read-only). Everything else — `~/.ssh`, `~/.aws`, `~/.npmrc`, `~/.contenox`, the rest of the disk — is unreachable.
- **Exec** — the agent can run only programs it is allowed to reach.
- **Environment** — the agent gets a scrubbed, minimal environment with a scoped `$HOME`, so no inherited credential rides along.

This runs on any Linux host, needs no setup, and requires no elevated privilege. The network is left open in this mode: the agent can reach the network (it needs to, to reach its model), but it cannot touch your filesystem or run outside its box. Use this default when the agent is trusted to use the network and you only need to keep it off your files and credentials.

## What the wall does not confine: contenox itself

The wall confines code contenox did not write. It does not apply to contenox re-invoking its own binary — a chain unit, or a mission unit the fleet dispatches, which is `contenox` bound to a chain file. Such a unit is recognized by file identity (the command it names must be this running executable; a copy or a rename does not qualify) and is spawned outside the wall.

A unit like this shares the one runtime state — the same database, the same seeded presets, the same workspace — and the wall denies exactly that state (`~/.contenox` is never carved out, so an agent can never reach the policy that governs it). Running as contenox only grants the right to run as contenox: what the unit then does runs through the runtime's own tool grants and the [human-in-the-loop gate](/docs/guide/hitl/), which is the layer built to govern it.

The same applies to every chain contenox runs in its own process — a `contenox serve` session, an editor `acp`/`acpx` session. `local_shell` in those sessions is **not confined** by this wall: contenox forwards the command to the ACP client's `terminal/*` capability rather than spawning it itself, so it runs as an ordinary child process of whatever is on the other end of that connection, free of the filesystem, exec, and environment fence a foreign agent gets. What governs a chain's shell access is the approval gate and the chain's tool policy — a gate at the tool layer, not a wall at the kernel. Give an untrusted driver the hardened `acpx` profile (`local_shell` denied outright) rather than assuming a sandbox that is not there.

Because that gate is a tool gate, it decides on the *name* a call passes it. A policy that allows `go build` without asking allows whatever `go` resolves to, and the wall is not there to catch a substituted one. [Trusted binaries](/docs/guide/trusted-binaries/) closes that at the gate instead — pinning allowlisted names to an absolute real path and a SHA256 — and its per-platform matrix says what that delivers where the Landlock fence below does not exist (macOS and Windows).

## Confining the network too (the opt-in wall)

To restrict which hosts the agent may reach — so it can talk to its model but cannot fetch arbitrary URLs or exfiltrate through its own HTTP client — turn on the network wall.

There is no global on/off flag. You enable the network wall by naming the hosts the agent legitimately needs. Naming even one host turns the wall on and refuses every host you did not name.

### Whitelist allowed domains

Edit `~/.contenox/sandbox-carveouts.json`. It is deny-default: absent or empty means the FS/exec fence only, network open. Unknown fields are rejected, and every hole must state why it exists.

```json
{
  "filesystem": [
    { "path": "~/.claude", "mode": "ro", "needs": "agent auth/config to start" }
  ],
  "network": [
    { "host": "api.anthropic.com", "needs": "the agent's model endpoint" },
    { "host": "registry.npmjs.org", "ports": [443], "needs": "npm install fetch" }
  ]
}
```

- Each `network` entry needs a `host` and a `needs` (a short reason the agent breaks without it). `ports` is optional and narrows the hole to specific destination ports; omit it to allow the host on any port.
- Every host not listed is refused and logged. Listing any network host enables the wall.
- `filesystem` works the same way: add read (`"mode":"ro"`) or write (`"mode":"rw"`) holes beyond the defaults, each justified. Keep them read-only unless a read-only hole demonstrably breaks the agent.

For a fully offline agent (no network at all), set `CONTENOX_SANDBOX_NETWORK_WALL=1` on the serve process and leave `network` empty — the wall builds with no route.

## Making the network wall work on Ubuntu

The network wall puts the agent in a private user and network namespace, so the network is absent by construction. Building that namespace without root requires **unprivileged user namespaces** — the one thing a default Ubuntu box does not allow.

Ubuntu 23.10+/24.04 restrict unprivileged user namespaces by default (`kernel.apparmor_restrict_unprivileged_userns = 1`). A freshly installed `contenox` binary has no AppArmor profile granting the exception, so on a stock Ubuntu host the network wall cannot be built and the agent fails to start — fail-closed by design.

To allow it, pick one:

- **Relax the restriction host-wide** (simplest):
  ```
  sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
  ```
  Persist it by dropping `kernel.apparmor_restrict_unprivileged_userns = 0` into a file under `/etc/sysctl.d/`.
- **Ship an AppArmor profile** that grants only the contenox binary `userns create`, keeping the restriction on for everything else on the host.

This requirement applies only to the network wall. The default filesystem/exec/environment fence uses no namespaces and no kernel-namespace privilege, so it runs on stock Ubuntu unchanged. If you do not need per-host network confinement, none of this applies to you.

### Troubleshooting

- Agent fails to start with an "unprivileged userns disabled" error, or a downstream "connection closed" right after you add `network` carve-outs → this is the Ubuntu userns restriction. Apply one of the fixes above, or remove the `network` carve-outs to fall back to the FS/exec fence (network open, no privilege needed).
- Non-Linux hosts have no wall — the confinement mechanisms are Linux-only.
