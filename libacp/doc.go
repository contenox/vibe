// Package libacp implements the Agent Client Protocol (ACP) v1, the
// JSON-RPC-over-NDJSON protocol editors and agents use to talk to each
// other. It supports both roles: the agent side, implementing [Agent] (or
// embedding [UnimplementedAgent]) and serving it via
// [NewAgentSideConnection]; and the client side, implementing [Client] (or
// embedding [UnimplementedClient]) and driving an agent via
// [NewClientSideConnection]. Both share the same wire machinery: NDJSON
// framing, request-id correlation, per-request cancelable contexts honoring
// "$/cancel_request", panic-safe handler dispatch, and extension-method
// passthrough.
package libacp
