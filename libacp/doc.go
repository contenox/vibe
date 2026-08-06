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
//
// A connection reads from any io.ReadWriteCloser; the subpackage
// github.com/contenox/contenox/libacp/acpexec spawns an agent subprocess over
// stdio and hands back the transport.
//
// Client-role usage — spawn, connect, initialize, open a session, prompt:
//
//	proc, err := acpexec.Spawn(ctx, exec.Command("contenox", "acp"))
//	if err != nil {
//		return err
//	}
//	conn := libacp.NewClientSideConnection(proc, func(*libacp.ClientSideConnection) libacp.Client {
//		return myClient{} // embeds libacp.UnimplementedClient
//	})
//	go conn.Run(ctx)
//
//	if _, err := conn.Initialize(ctx, libacp.InitializeRequest{
//		ProtocolVersion: libacp.ProtocolVersion,
//		ClientInfo:      &libacp.Implementation{Name: "my-editor", Version: "1.0"},
//	}); err != nil {
//		return err
//	}
//
//	sess, err := conn.NewSession(ctx, libacp.NewSessionRequest{Cwd: "/abs/path/to/project"})
//	if err != nil {
//		return err
//	}
//
//	resp, err := conn.Prompt(ctx, libacp.PromptRequest{
//		SessionID: sess.SessionID,
//		Prompt:    []libacp.ContentBlock{libacp.NewTextContent("hello")},
//	})
//	_ = conn.CancelPrompt(sess.SessionID) // cancel the in-flight turn from another goroutine
package libacp
