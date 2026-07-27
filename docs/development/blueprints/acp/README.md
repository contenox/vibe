# ACP Blueprints

Contenox is an ACP hub: `contenox acp` serves the Agent Client Protocol over
stdio upward, for editor and desktop clients (Zed, JetBrains, AionUi, OpenClaw)
and the Beam TUI;
`contenox acpx` runs the headless / untrusted-driver profile. The same Go
runtime is also an ACP client downward, driving other ACP agents (including
other contenox instances) as declared external agents and fleet units. These
docs cover both directions, plus the fleet/mission machinery built on them.

| Doc | Status | What it covers |
| --- | --- | --- |
| [acp-client-engine.md](acp-client-engine.md) | direction | Contenox as an ACP client: the models/tools/agents ladder, ACP-as-taskengine-step and ACP-as-modelprovider, the provider honesty rule, the permission-routing invariant, the shared `libacp` client-core prerequisite, and the agent registry pattern |
| [agent-sandbox.md](agent-sandbox.md) | design record | The agent sandbox security architecture |
| [mission-plans.md](mission-plans.md) | building | The plan engine as a resident planner: plan revisions, step progress, prompt surface |
| [envelope-compute-bounds.md](envelope-compute-bounds.md) | building | The mission envelope as a unit's TOTAL boundary: compute bounds (turns, tool calls, tokens) alongside HITL action bounds |
| [registry-submission/](registry-submission/README.md) | artifacts | The `agent.json` + icon to copy into an `agentclientprotocol/registry` fork, with validation steps |

Related: the ACP slash-command surface documented in
`docs/reference/contenox-cli.md`, and the serve-era fleet/mission design record
in the fleet-consolidation design record (git history; summarized in [Retired R&D](../retired/README.md)).
