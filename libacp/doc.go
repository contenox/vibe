// Package libacp implements the Agent Client Protocol (ACP) v1, the
// JSON-RPC-over-NDJSON protocol editors and agents use to talk to each other. It
// supports both roles: [Agent] served via [NewAgentSideConnection], and [Client]
// driving an agent via [NewClientSideConnection].
package libacp
