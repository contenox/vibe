// Package agentinstance is the runtime's backend-owned agent-instance kernel:
// it spawns and OWNS running agent instances (ACP subprocesses — somebody
// else's agent, or this binary's own ACP server bound to one of the runtime's
// task chains), decoupled from any client connection,
// and lets many viewers ATTACH to one instance's sessions to observe its
// stream and — for exactly one controller per session — answer its permission
// requests.
//
// # Why the runtime owns instances
//
// An external ACP agent subprocess used to be spawned bound to a per-connection
// context (runtime/acpsvc's Transport) and died the moment that connection
// dropped. This kernel relocates that ownership to the RUNTIME: a Manager spawns
// an instance bound to its own long-lived root context, so the instance lives
// for the Manager's lifetime and clients ATTACH to it rather than owning it.
// When the Manager is Closed (runtime shutdown) every instance is torn down.
//
// # Two layers
//
// The package is split into two layers — an instance primitive and the
// orchestration logic around it:
//
//   - Layer A — the instance primitive (instance.go, viewer.go, journal.go,
//     drive.go). An ACP-generic primitive depending only on libacp +
//     runtime/agenthost. It owns its own INTERNAL journaling harness (the
//     libacp.Client wired into the downstream connection), a bounded per-session
//     JOURNAL of session/update events, a per-session VIEWERS registry with one
//     CONTROLLER each, a watchDog that applies a restart policy when the downstream
//     dies, AND the complete downstream-DRIVING behavior (drive.go): the
//     initialize-once handshake, session/new, session/prompt, session/cancel,
//     set_config_option / set_mode / set_model, and the per-session capture of the
//     downstream's config-option / mode / model / slash-command surface. It knows
//     nothing about the registry, HTTP, or acpsvc.
//
// # The kernel drives; transports consume
//
// The kernel owns the COMPLETE behavior of running AND driving an ACP agent. A
// transport (acpsvc, a future CLI, a scheduler) is a THIN consumer: it calls the
// Manager's session-driving API (OpenSession, Prompt, Cancel, CloseSession,
// SetConfigOption, and the SessionConfigOptions / AvailableCommands accessors) and
// OBSERVES a session's stream by attaching viewers — it holds NO kernel logic and no
// raw downstream connection. The downstream protocol driving that once lived in a
// transport (acpsvc/external.go reaching into a raw connection) is HERE now. The
// kernel journals the raw downstream session/update stream and captures the driving
// surface into per-session state (the synthetic mode/model→config-option mapping
// included); how a transport presents that surface upstream, and any durable
// transcript, remain the transport's concern (the journal is live/in-memory).
//
//   - Layer B — the Manager (manager.go). The orchestration layer: it resolves
//     DECLARED agents by name via agentregistryservice, brings up instances
//     wired to a lifecycle EVENT SINK (the substrate a future scheduler and
//     beam's fleet view both hang off), and joins declared-agent config with
//     live instance status in List.
//
// # Where ACP forced a divergence from a byte-terminal process manager
//
//   - Structured events, not bytes. The journal holds libacp.SessionNotification
//     values, not a byte scrollback, so replay reconstructs a viewer's exact
//     event history rather than a terminal's visual state.
//   - Bidirectional, not one-way. A pty fans bytes out to viewers only; an ACP
//     downstream ALSO calls BACK (session/request_permission and the terminal/*
//     family). Those inbound requests are routed to the session's single controller
//     viewer — the reason Viewer carries RequestPermission, not just Deliver, and the
//     reason a controller MAY implement the optional TerminalServer to service
//     terminal/* (a controller that does not gets MethodNotFound). The kernel only
//     ROUTES these callbacks; it has no shell dependency. Whether the downstream is
//     told terminals exist is negotiated at OpenSession per SessionSpec.Terminal.
//     A session with NO controller — the headless case a dispatched unit runs in by
//     design — denies by default, or hands the request to an INJECTED answerer when
//     one is wired (Manager.WithPermissionFallback). That seam keeps the kernel
//     policy-free: it neither knows nor constrains how the answer is reached.
//   - Restart loses conversation context. A byte-terminal manager restarts a
//     crashed process and the process resumes from its own on-disk state. An ACP restart
//     re-spawns a fresh subprocess that must be re-Initialized; the downstream
//     agent's in-memory conversation/session context is LOST. Restart keeps the
//     fleet alive, not the conversation — see instance.watchDog.
//   - Control is attach-bound, not time-leased. A wall-clock control lease
//     exists to reclaim control from a client that
//     vanished without releasing. Viewers here attach/detach explicitly and
//     reliably through the Manager, so control is bound to attachment (auto-
//     promoted on controller detach), not a wall-clock lease — a time lease would
//     wrongly deny permissions while a live controller is still attached.
package agentinstance
