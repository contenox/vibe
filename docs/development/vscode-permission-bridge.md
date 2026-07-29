# VS Code ACP-Shaped Permission Bridge Plan

> **Status:** implemented — landed as `internal/surfaces/vscodeagent/approval.go` (ApprovalBroker) over `session/request_permission`; kept as the design record.

## Problem

The VS Code bridge currently treats runtime approvals as a notification plus a later `approvalRespond` request. That is too weak for Contenox HITL semantics. If the editor fails to render or answer the approval UI, the runtime can keep reasoning and choose another tool path that the active policy may allow. The approval boundary is therefore not represented as a blocking protocol operation.

The fix is not to hardcode tool names in the extension. Tool identity and arguments already exist in the runtime `hitlservice.ApprovalRequest` and task events. The VS Code extension should render the permission request it receives and return a decision. The runtime remains the policy owner.

## ACP Lessons To Reuse

- `libacp.AgentSideConnection.call` is the correct shape for permission flow: allocate a request id, store a pending response channel under a mutex, write the request, block on response/context/connection close, and remove the pending entry on every exit path.
- `session/request_permission` is a reverse request from agent/runtime to client/editor. It carries `sessionId`, a structured `toolCall`, and explicit `options`.
- `internal/surfaces/acpsvc.Transport.AskApproval` converts a Contenox `ApprovalRequest` into ACP `RequestPermissionRequest`, calls `RequestPermission`, and treats cancelled or malformed outcomes as not approved.
- `permPending` in `internal/surfaces/acpsvc` suppresses normal tool-call updates while a permission card owns that tool call. This avoids a pending/completed tool card racing or replacing the approval UI.
- `normalizeToolCallNotification` preserves monotonic tool status. A later `pending` update must not rewind an already in-progress/completed card.
- ACP does not require the editor to know the complete tool catalog. It renders the concrete tool call supplied by the runtime.

## Target VS Code Bridge Dialect

Keep `contenox vscode-agent --stdio` and the current Content-Length JSON-RPC framing for the extension process. Replace the active approval subprotocol with an ACP-shaped reverse JSON-RPC request:

```json
{
  "jsonrpc": "2.0",
  "id": 42,
  "method": "session/request_permission",
  "params": {
    "sessionId": "contenox-session-id",
    "toolCall": {
      "toolCallId": "call-1",
      "title": "local_shell.local_shell: python3",
      "kind": "execute",
      "status": "pending",
      "rawInput": {"command": "python3"}
    },
    "options": [
      {"optionId": "allow", "name": "Allow", "kind": "allow_once"},
      {"optionId": "deny", "name": "Deny", "kind": "reject_once"}
    ]
  }
}
```

The extension responds:

```json
{"jsonrpc":"2.0","id":42,"result":{"outcome":{"outcome":"selected","optionId":"allow"}}}
```

or:

```json
{"jsonrpc":"2.0","id":42,"result":{"outcome":{"outcome":"cancelled"}}}
```

## Implementation Steps

1. Add shared ACP permission builders in Go so `acpsvc` and `vscodeagent` use the same `libacp.RequestPermissionRequest` construction for titles, raw input, diff content, and tool kind.
2. Add server-initiated request support to `internal/surfaces/vscodeagent.Server`: numeric ids, pending response map, response routing in `Run`, context cleanup, and fail-closed behavior on write/close/cancel.
3. Change `ApprovalBroker` to call `session/request_permission` synchronously instead of sending `approvalRequested` notifications. Delete the old `approvalRespond` compatibility path once the TypeScript bridge uses the blocking request.
4. Add VS Code `BridgeClient` handling for incoming JSON-RPC requests. Dispatch `session/request_permission`, call an active chat approval handler, and send a JSON-RPC response. If no handler exists or the UI fails, answer `cancelled`.
5. Wire the chat turn's `toolInvocationToken` into the active approval handler for the duration of the turn. This keeps native VS Code approval UI tied to the current chat request and prevents approvals outside a chat turn from silently succeeding.
6. Add a VS Code agent permission-pending guard mirroring ACP so normal `toolCall` notifications do not render over the approval request for the same session/tool call.
7. Patch shipped interactive HITL policies so `local_shell.local_shell` defaults to `approve`, not `allow`. A permissive shell fallback defeats the whole HITL boundary.
8. Add focused tests:
   - Go: server-initiated request routing, approval allow/deny/cancel, close/cancel cleanup, and tool-call suppression while permission is pending.
   - TypeScript: `BridgeClient` handles `session/request_permission`, responds selected/cancelled, and fails closed without a handler.
   - Policy: dev/default/acp policies do not auto-allow a plain shell command such as `python3`.

## Manual Smoke Test

1. Install/reload the extension build.
2. Select an interactive HITL policy.
3. Ask `@contenox` to create a file in the workspace. A permission card should block before execution.
4. Deny it. The file must not appear, and the assistant must report that the tool was denied.
5. Ask `@contenox` to create a file in `/tmp` or `$HOME`. A shell fallback must also ask for approval, not silently run `python3`.
6. Check `~/.contenox/vscode-telemetry.log` for `runtime.server_request.start`, `chat.approval.requested`, and `chat.approval.responded` entries.

## Non-Goals

- Do not replace `vscode-agent --stdio` with the full ACP process in this pass.
- Do not make the extension discover and enforce the tool catalog. Policy and tool identity stay in the runtime.
- Do not rely on telemetry as enforcement. Telemetry proves what happened; the blocking request is the enforcement.
