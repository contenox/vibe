# The ACP client library and its verification harnesses

`libacp` implements the Agent Client Protocol (ACP) v1 for **both roles** of
the conversation:

- **Agent side** — `libacp.AgentSideConnection` serving a `libacp.Agent`.
  This is the upward direction: `internal/surfaces/acpsvc` implements the production
  agent that ACP clients (Zed, JetBrains, the beam TUI) drive via
  `contenox acp`.
- **Client side** — `libacp.ClientSideConnection` driving a `libacp.Client`.
  This is the downward direction: contenox itself opening sessions on other
  ACP agents.
  The client exposes every agent-bound method (`Initialize`, `NewSession`,
  `Prompt`, `CancelPrompt`, session config options, ext-method passthrough)
  as outbound calls, receives streamed `session/update` notifications in
  wire order, and answers the agent's reverse calls
  (`session/request_permission`, `fs/*`, `terminal/*`).

The package documentation (`libacp/doc.go`) carries a compact end-to-end
client example. `libacp/acpexec` provides the subprocess-over-stdio
transport both roles use to reach a peer binary.

## E2E harnesses against the Rust reference SDK

The in-repo tests exercise both halves against each other (in-process fakes,
plus a production loopback in `internal/surfaces/acpsvc/client_loopback_test.go` that
runs the real `acpsvc` agent against the real `ClientSideConnection`). Two
additional, opt-in harnesses validate each role against **independently
implemented** peers from the reference Rust SDK
([github.com/agentclientprotocol/rust-sdk](https://github.com/agentclientprotocol/rust-sdk)):

| Target | Validates | Peer binary |
| --- | --- | --- |
| `task acp-conformance` | the **agent** side (`libacp/cmd/acp-stub-agent` via `AgentSideConnection`) | `acp-validator`, a conformance-checking ACP client |
| `task acp-client-e2e` | the **client** side (`ClientSideConnection` over `acpexec`) | `testy`, the SDK's deterministic test agent |

Both targets skip-or-fail cleanly when their binary env var is unset:

- `ACP_TESTY_BIN` / `ACP_MCP_ECHO_BIN` — build from a rust-sdk checkout with
  `cargo build -p agent-client-protocol-test --bins`; the binaries land at
  `<checkout>/target/debug/testy` and `<checkout>/target/debug/mcp-echo-server`.
  (`ACP_MCP_ECHO_BIN` only gates the MCP pass-down test, which skips on its
  own if unset.)
- `ACP_VALIDATOR_BIN` — the validator is not part of the SDK; its source is
  vendored in [`tools/acp-validator/`](../../tools/acp-validator/README.md)
  in this repository. It depends on the SDK's `agent-client-protocol` crate
  by relative path, so copy it next to a rust-sdk checkout and `cargo build`
  there (the README has the exact steps). `ACP_YOPO_BIN` (optional,
  additional client) builds from the rust-sdk's `src/yopo`.

## The composed host e2e (registry → agenthost → live turn)

One layer above the wire-dispatch harnesses, `task acp-host-e2e` validates the
runtime's **client-host composition** end to end: an `agents` row created and
resolved through the real registry service, spawned and driven by
`internal/services/agenthost.DriveTurn` (initialize → session/new → session/prompt →
teardown), with the streamed reply asserted on the caller's harness.
Servers, each isolating something different:

| Server | Gate | Asserts |
| --- | --- | --- |
| `acp-stub-agent` (hermetic) | none — runs in plain `go test` | deterministic "ack" turn, update ordering through the harness seam |
| contenox self-loopback (`contenox acp` built in-test, driving a no-model chain fixture) | none (skips under `-short`) | byte-exact fixture reply through registry → host → real chain |
| `testy` | `ACP_TESTY_BIN` | deterministic echo/greet through the composed path |
| Claude Code (via `claude-code-acp`) | `ACP_CLAUDE_ACP_BIN` — never CI; needs Claude credentials | turn **shape** only: `end_turn` plus displayable output from a real, foreign production agent |

The user-facing twin of this harness is `contenox agent check <name>`: it
drives the same DriveTurn path against any registered agent and streams the
reply — the way to verify an agent right after `contenox agent add`.

### MCP forwarding and the agent's command surface

An agent row's `mcp_servers` config field is an explicit, per-agent allowlist
of registered MCP server names (`contenox mcp list`) forwarded to that agent
in ACP `session/new` — the mirror of what `internal/surfaces/acpsvc` consumes when
contenox is on the *agent* side of the same exchange. The host
(`agenthost.ResolveForwardedMcpServers` + DriveTurn) resolves names loudly
(a missing name fails the turn rather than silently shrinking the agent's
declared context), filters by the agent's initialize-advertised
`mcpCapabilities` (stdio is baseline; http/sse gated), and reports
forwarded-vs-dropped on the TurnResult. Contenox-side auth synthesis
(authToken/authEnvKey/oauth/injectParams) is never translated into the
payload; forwarding a server at all is the consent boundary. The composed
pass-down is pinned by `TestHostE2E_Testy_McpPassDownThroughComposedPath`
(testy connects to the forwarded `mcp-echo-server` and lists its tools).

The slash commands a hosted agent advertises (`available_commands_update`)
are recorded by the harness (`RecordingHarness.AvailableCommands`) and
printed by `agent check`. *Merging* them with contenox's own acpsvc command
set is not implemented: acpsvc's leading-slash interception is the natural
merge point, and a collision policy is required since command sets can
overlap (claude-code-acp and acpsvc both advertise `/compact`).
