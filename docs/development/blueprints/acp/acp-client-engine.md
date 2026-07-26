# Blueprint: Contenox as an ACP Hub — the client-side engine capability

Contenox sits on both sides of the Agent Client Protocol. Upward, `acpsvc` is
an ACP **agent**: editors, the Beam TUI, and any conformant ACP client drive
it over stdio (`contenox acp` / `contenox acpx`). Downward, the Go runtime is
an ACP **client**: the engine opens sessions on *other* ACP agents and drives
them the same way Zed drives contenox.

The downward direction is not an app and not a UI feature. It is a
capability of the Go runtime itself, reachable from two places, both
sanctioned, neither prioritized over the other:

- **(a) ACP as a taskengine step** — a chain task that opens and drives a
  session on an external ACP agent.
- **(b) ACP as a modelprovider implementation** — an external ACP agent
  registered so it is selectable anywhere a model goes.

Both are thin adapters over one shared prerequisite: a client-side connection
core in `libacp`. This document specifies the hub architecture, the two
shapes, the invariant that makes either of them safe, and the registry that
names the agents on the other end.

## The ladder: models, tools, agents

The taskengine already composes two kinds of capability into a chain. ACP
adds a third:

| Primitive | Example | Statefulness | Side effects | Reached via |
| --- | --- | --- | --- | --- |
| **Model** (provider) | a cloud or local LLM | stateless per call | none — text in, text out | `modelrepo.Provider` / `llmrepo.ModelRepo` |
| **Tool** | `local_shell`, `local_fs`, an MCP server | stateless per call, gated per-call | yes, individually HITL-gated | `localtools` / MCP worker |
| **Agent** (ACP) | an external coding/ops agent | **stateful** — a session with turns, plans, and its own tool calls | yes, but *bundled*: a whole session's worth of tool calls behind one driven interaction | libacp client core (this document) |

Models and tools are both already first-class taskengine primitives; an agent
is qualitatively different because a single interaction with it can contain an
arbitrary number of the driving agent's own tool calls and permission
requests. That difference is why shape (b) needs an explicit honesty rule
(below) and why shape (a) is the structurally safer default.

## Shape (a): ACP as a taskengine step

A chain task type that opens a session on a configured external agent,
prompts it, and surfaces the session's protocol events — plans, tool calls,
`session/request_permission` — into chain semantics rather than swallowing
them into one opaque result.

This is protocol-honest by construction: taskengine's step-sequencing already
threads one step's output into the next (`ExecEnv`'s loop in
`internal/kernel/taskengine/taskenv.go` assigns the prior step's output as
`taskInput` before invoking the next task), and `Transition.Branches` already
picks the next task
by evaluating that output. An ACP step's final turn is just another step
output subject to the same branching — so a **guardrail step can sit between
two agent actions** exactly the way it sits between any other two tasks today.
Fan-out (running the same ACP step against several agents, or several
sessions) follows the same pattern any other task type uses; nothing about
agents requires new fan-out machinery.

Concretely, this is a new `TaskHandler` (see `internal/kernel/taskengine/tasktype.go`
for the existing closed enum — `raise_error`, `route`, `chat_completion`,
`execute_tool_calls`, `noop`, `tools`) and a new case in the `taskexec.go`
switch, not a plugin/registry layer. That matches the taskengine's existing
architecture rather than inventing indirection ACP does not need.

## Shape (b): ACP as a modelprovider implementation

Register an external ACP agent so it is selectable anywhere a model is
selectable today — chat, routing, config options — by implementing the
provider client interfaces over an ACP session.

### The provider honesty rule

`modelrepo`'s actual client interfaces are literally stateless:

```go
type LLMChatClient interface {
    Chat(ctx, messages []Message, args ...ChatArgument) (ChatResult, error)
}
type LLMPromptExecClient interface {
    Prompt(ctx, systemInstruction string, temperature float32, prompt string) (string, error)
}
```

(`internal/models/modelrepo/modelprovidertypes.go`). Implementing `Chat` over an ACP
agent means, inside one call: open or resume a session, send the prompt,
absorb every `tool_call`, every `plan` update, and every
`session/request_permission` round-trip the agent produces, wait for
`stopReason`, and return the final message text as if it were a stateless
completion. Everything that happened in between — files edited, commands run,
services restarted — is invisible to the chain that called `Chat`.

That is semantically leaky by default, and it is leaky in exactly the way the
chat-on-ACP doctrine (the retired beam-on-acp record, in git history) exists to prevent,
aimed in the opposite direction: a chain step is supposed to be a reviewable unit whose
guardrail evaluates its *complete* output before anything proceeds (blocking
whole-message output is the architecturally
honest default). A provider call that silently performed side effects defeats
that guarantee — the guardrail reviews text, not the actions that produced it.

**Rule: agent-as-provider is only valid under a permissions policy of
deny/read-only.** Used this way it is "a frontier agent as a pure reasoning
model" — no file writes, no command execution, no side effects — and the
provider abstraction stays honest. Side effects must never hide inside a
provider call. An ACP agent registered as a provider with any looser policy is
a defect, not a feature; if the work genuinely needs side effects, it belongs
in shape (a), where the chain sees every action as it happens.

## Permission routing — the load-bearing seam

Both shapes eventually produce the same reverse call: the driven agent's
`session/request_permission`. How that gets answered is the first design
decision of the client core, and it is load-bearing:

**A sub-agent's `session/request_permission` must be answered by contenox's
own HITL policy machinery** — the same policy evaluator that gates contenox's
own tools when contenox is the agent (`internal/services/hitlservice/policy.go`:
`Policy{DefaultAction, Rules}`, first-match `Rule{Tools, Tool, When
[]Condition, Action}` with condition operators including `glob`,
`command_blacklist`, `command_ask_always`). The policy answers on the human's
behalf where the rule allows it, and escalates to the human through contenox's
own approval flow otherwise — never a separate, ad hoc path bolted onto the
client core.

**Invariant: if permission routing degrades into auto-approve — "skip HITL,
it's just a sub-agent" — the entire governance value of driving that agent is
gone.** The same failure mode the chat-on-ACP doctrine polices for inbound
approvals (a permission request rendered honestly, with the rule that gated it
nameable) applies outbound: an operator must always be able to name which
policy gated the sub-agent's last action.

## The shared prerequisite: a client-side connection core in `libacp`

Both shapes live on this core. `libacp` — the runtime's own ACP
implementation — speaks both sides: `AgentSideConnection` (`libacp/conn.go`)
and the `Agent` / `AgentFactory` contract it drives (`libacp/agent.go`),
which is exactly what `acpsvc` wraps (`acpsvc.New(deps) libacp.AgentFactory`,
`internal/surfaces/acpsvc/transport.go`), and `ClientSideConnection`
(`libacp/clientconn.go`) with the `Client` / `ClientFactory` contract
(`libacp/client.go`) — session lifecycle (new/load/resume/prompt/cancel), the
pending-request map, and the permission callback, shared machinery the two
shapes sit on top of as thin adapters.

Why one shared core matters: the retired VS Code bridge hand-rolled its own
pending-request bookkeeping (a request-id-keyed map, resolved when the peer's
response arrives) on a bespoke stdio protocol — the same shape the client core
provides for `session/request_permission`, `fs/read_text_file`, and
`terminal/create`. Solving it again for real external ACP agents, bespoke and
non-reusable, would repeat that mistake; any future outbound transport dial
likewise reuses the core rather than adding another implementation.

Symmetry worth noting: `acpsvc`'s own `local_shell` handling already expects
capable peers to run commands on the peer's own machine — it routes through
the *client's* `terminal/create` when the connected client advertises the
Terminal capability, falling back to server-side exec only when the peer
cannot (`internal/surfaces/acpsvc/commandrunner.go:35`, `if
!t.getClientCaps().Terminal { ... fallback ... }`). The client core built here
is the mirror image of that same capability negotiation, generalized to
contenox acting as the capable peer for a sub-agent it drives.

## The agent registry

External ACP agents are registered the same way external MCP servers already
are — a persisted, name-keyed spawn spec, not a bespoke config surface. The
existing pattern is `runtimetypes.MCPServer` (`internal/store/runtimetypes/mcp.go`):
`ID`, `Name`, `Transport`, `Command`, `Args`, plus connection/auth fields
(`URL`, `AuthType`, `AuthToken`, `AuthEnvKey`, `OAuthClientID`,
`OAuthClientSecretEnv`, `ConnectTimeoutSeconds`, `Headers`,
`InjectParams`), with CRUD (`CreateMCPServer`, `UpsertMCPServerByName`,
`GetMCPServer`, `ListMCPServers`, …) already built. An agent registry table
should follow the same persistence shape: name-keyed upsert, CRUD, a
spawn/connect spec.

One nuance worth carrying over precisely rather than assumed: `runtimetypes.
MCPServer` represents environment injection narrowly (`AuthEnvKey` names a
single env var to read a secret from), not as a general env map. `libacp`'s
own wire-level MCP type (`libacp/mcp.go`, `McpServer{Type, Name, Command,
Args, Env []EnvVariable, URL, Headers}`) already carries the ACP-spec-literal
`{name, command, args, env[], url, headers}` shape, because relaying MCP
servers to a peer routinely needs several environment variables at once. An
agent registry — spawning an external ACP agent process, which just as
routinely needs multiple env vars (API keys, config paths, credentials) — should
follow `libacp.McpServer`'s fuller `Env` list rather than replicate the
narrower `AuthEnvKey` shorthand.

## Contenox driving contenox — remote nodes as a first-class composition

The client core does not assume the peer is a third-party agent. The most
important instance of shape (a) may be contenox driving another instance of
itself: an engine on a laptop opening a session on a `contenox acp`/`acpx`
process running on a different host, exactly as it would open a session on
Zed's or Claude Code's agent. Nothing about the client core, the permission
routing invariant, or the registry pattern is special-cased for "another
contenox" — that uniformity is the point. This is also the composition a
remote-operations surface would be built on: a
device-owning host drives a remote, operated host's `contenox acp` over the
same client core that drives any other registered agent, with the same
permission-routing invariant applying to the remote host's own HITL policy.

## Invariants (anti-patterns to reject in review)

- **An agent-as-provider that executes side effects.** Shape (b) is only
  honest under a deny/read-only permission policy; anything looser belongs in
  shape (a), where the chain sees the actions.
- **Auto-approve as a default or fallback permission policy for a driven
  agent.** Permission routing must reach contenox's HITL policy machinery,
  never a bypass justified by "it's just a sub-agent."
- **A second hand-rolled ACP client RPC layer outside `libacp`.**
  Replicating the retired VS Code bridge's bespoke pending-request map instead
  of extending the shared core repeats a mistake already made once.
- **A chain step or provider that cannot name which sub-agent, session, or
  policy backs it.** The operator must always be able to answer "what is this
  and what rule gated its last action," outbound as well as inbound.
- **Treating shape (a) and shape (b) as competing** rather than as two thin
  adapters over the same client core. Divergent session or permission handling
  between them — one honoring the HITL policy machinery, the other rolling its
  own — is a defect, not a design choice.
- **A registry entry with no invocable spawn spec** — an agent listed without
  a `{name, command, args, env}`-shaped way to actually reach it duplicates the
  MCP registry's own early mistakes instead of reusing the pattern.
