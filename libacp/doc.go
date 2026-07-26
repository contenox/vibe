// Package libacp implements the Agent Client Protocol (ACP) v1 — the
// JSON-RPC-over-NDJSON protocol editors and agents use to talk to each other
// — for both roles:
//
//   - The agent side: implement [Agent] (or embed [UnimplementedAgent]) and
//     serve it with [NewAgentSideConnection]. This is what `contenox acp`
//     does; see runtime/acpsvc for the production implementation.
//   - The client side: implement [Client] (or embed [UnimplementedClient])
//     and drive an agent with [NewClientSideConnection]. The client receives
//     streamed session/update notifications through [Client.SessionUpdate]
//     and answers the agent's reverse calls (session/request_permission,
//     fs/read_text_file, fs/write_text_file, terminal/*).
//
// Both connection types share the same wire machinery: NDJSON framing,
// request-id correlation, per-request cancelable contexts honoring
// "$/cancel_request", panic-safe handler dispatch, and extension-method
// passthrough ([AgentSideConnection.SetExtRequestHandler],
// [ClientSideConnection.CallExtMethod], and their mirrors).
//
// A connection reads from any io.ReadWriteCloser. For the common case of an
// agent subprocess spoken to over stdio, the subpackage
// github.com/contenox/beam/libacp/acpexec spawns the process and hands
// back the transport.
//
// # Driving an agent (client role)
//
// The essential client flow — spawn, connect, initialize, open a session,
// prompt, cancel:
//
//	proc, err := acpexec.Spawn(ctx, exec.Command("contenox", "acp"))
//	if err != nil {
//		return err
//	}
//	conn := libacp.NewClientSideConnection(proc, func(*libacp.ClientSideConnection) libacp.Client {
//		return myClient{} // embeds libacp.UnimplementedClient; overrides SessionUpdate, RequestPermission, ...
//	})
//	go conn.Run(ctx) // serves the connection until ctx ends or the transport closes
//
//	if _, err := conn.Initialize(ctx, libacp.InitializeRequest{
//		ProtocolVersion: libacp.ProtocolVersion,
//		ClientInfo:      &libacp.Implementation{Name: "my-editor", Version: "1.0"},
//	}); err != nil {
//		return err
//	}
//
//	sess, err := conn.NewSession(ctx, libacp.NewSessionRequest{
//		Cwd:        "/abs/path/to/project",
//		McpServers: []libacp.McpServer{}, // MCP servers to hand down to the agent
//	})
//	if err != nil {
//		return err
//	}
//
//	// Prompt blocks until the turn ends; streamed output arrives on
//	// myClient.SessionUpdate concurrently, in wire order.
//	resp, err := conn.Prompt(ctx, libacp.PromptRequest{
//		SessionID: sess.SessionID,
//		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("hello")},
//	})
//
//	// To cancel a turn from another goroutine while Prompt is in flight:
//	// sends session/cancel and auto-resolves the session's pending
//	// session/request_permission requests with the "cancelled" outcome, per
//	// the spec's cancellation contract. The Prompt call then resolves with
//	// StopReasonCancelled and a nil error.
//	_ = conn.CancelPrompt(sess.SessionID)
//	_ = resp
//
// # Serving an agent (agent role)
//
// The mirror image: implement [Agent], then
//
//	conn := libacp.NewAgentSideConnection(rw, func(c *libacp.AgentSideConnection) libacp.Agent {
//		return newMyAgent(c) // keeps c to send SessionUpdate and reverse calls
//	})
//	err := conn.Run(ctx)
//
// Handlers that must emit a session/update only after their own result is on
// the wire (e.g. available_commands_update after session/new) schedule it
// with [AfterResponse]. libacp/cmd/acp-stub-agent is a hermetic reference
// implementation of this role, used by the conformance harness.
//
// # Verification harnesses
//
// Beyond the in-process unit tests, each role is validated against the
// independently implemented Rust reference SDK
// (github.com/agentclientprotocol/rust-sdk): `make acp-conformance` runs the
// acp-validator conformance client (source in tools/acp-validator) against
// the stub agent, and `make acp-client-e2e` runs this package's client side
// against the SDK's deterministic "testy" agent over a real subprocess. See
// docs/development/acp-client.md.
package libacp
