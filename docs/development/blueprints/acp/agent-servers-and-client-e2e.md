# Blueprint: Agent servers and client-host e2e

**Status:** largely landed — the composed-path harness exists (`make acp-host-e2e`,
`internal/services/agenthost/e2e_{loopback,testy,claude,mcp}_test.go`) and `contenox agent
check` is its user-facing twin. Scopes how a declared agent is *served* as an ACP agent and
how the runtime's new **client-host** role (`internal/services/agenthost`) is verified
end-to-end. Sits on top of the landed external-agent plumbing (`agents` table,
`agentregistryservice`, `agenthost`) and the registration UX
([the `contenox agent` CLI](../../../reference/contenox-cli.md)). UI surfaces
are out of scope here.

## Problem

The client-host can spawn and drive an external ACP agent, but the only proof
today is a single `initialize` handshake against the hermetic Go stub. To *use*
and *trust* hosting a foreign agent, two things are missing:

1. **An always-available agent to drive** that is not a third-party install — so
   the path is exercisable in CI and by a developer with nothing installed.
2. **Independent conformance** — verification that contenox's *client* role is
   spec-correct on its own, not just "it works against our own agent side."

## The insight: contenox is already an ACP server

`contenox acp` runs the ACP **agent** role over stdio ("Run the Contenox ACP
server over stdio", `internal/surfaces/contenoxcli/acp_cmd.go`). It is the same `libacp`
JSON-RPC-over-`io.ReadWriteCloser` machinery the host uses, pointed the other way.
So contenox can be **registered as an external agent pointing at its own binary**
— a self-hosting loopback:

```
agenthost (client) → spawn `contenox acp` → contenox (agent) → a chain → reply
```

No external dependency, and it exercises the whole path: registry row → resolve →
`agenthost.Connect` → a live `session/new` + `prompt` → a real answer. It is also
the shape the future `chain` agent kind will take (a declared chain served as
`contenox acp --chain <id>`-style) — this blueprint does not build that kind, but
the loopback proves the seam.

Generalized: **any declared agent presents an agent-role server the host connects
to** — a foreign binary, the rust reference agent, or contenox itself. That
uniformity is what makes the host testable against several servers with one
harness.

## The servers we drive (pick per test intent)

| Server | What it is | What it isolates |
|---|---|---|
| `libacp/cmd/acp-stub-agent` | hermetic in-repo agent, no model | host spawn / connect / initialize / teardown (have this) |
| `testy` (agentclientprotocol/rust-sdk, `tools/rust-sdk`) | the reference SDK's deterministic conformance agent | contenox's **client** role is spec-correct, independent of contenox's agent side |
| `contenox acp` (self) | contenox's own agent role + a chain | the **full loopback**: registry → host → real ACP session → chain reply |

The rust reference tooling is already vendored and built
(`tools/rust-sdk/target/debug/{testy,yopo,mcp-echo-server}`,
`tools/acp-validator/target/debug/acp-validator`). Note the two directions the
reference tools cover, so we don't confuse them:

- **Agent side** (already covered by `make acp-conformance`): the rust
  *client*/validator drives contenox-as-agent.
- **Client side** (this blueprint): contenox-as-client drives the rust *agent*
  (`testy`) — the mirror, and the one that pins the new host.

## The e2e shape (extend what exists, don't reinvent)

`make acp-client-e2e` already drives `testy` through `libacp/acpexec`, gated on
`ACP_TESTY_BIN`, skipping cleanly when unset. Extend that one layer up — from
"libacp client dispatch" to "the composed registry+host":

1. **Register** the target as an `agents` row via the *manual* path
   (`contenox agent add <name> -- <command …>`), so the e2e needs no network
   registry fetch. Targets: `-- <testy-bin>`, `-- <stub-bin>`, or `-- contenox acp`.
2. **Resolve → `agenthost.Connect(ctx, harness)`** with a stub `libacp.Client`
   harness (`UnimplementedClient` or a small scripted one — real harness assembly
   is a later slice).
3. **Drive** a minimal exchange: `initialize` → `session/new` → one `prompt` →
   assert a terminal turn (`stopReason`, at least one message chunk). For the
   loopback, assert the chain's reply text; for `testy`, assert its deterministic
   scripted response.
4. **Tear down** and assert clean `Close()` / `Conn.Closed()`.

Gate each server's e2e on its binary env var (`ACP_TESTY_BIN`; the loopback needs
the freshly built `contenox` binary; the stub builds itself), matching the
existing conformance/client-e2e convention. Home is most naturally a new
`internal/services/agenthost` e2e (it is the integration point that composes
registry + host), with a `make` target beside `acp-client-e2e`.

