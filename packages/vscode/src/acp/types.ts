// Re-exports of @agentclientprotocol/sdk schema types needed outside acp/sdk.ts.
//
// The resolution-mode assertion (see sdk.ts) is only needed where the ESM-only
// package path is named directly. Once re-exported as local type aliases here,
// other CJS-compiled files can `import type { ... } from "./types"` (or
// "../acp/types") without repeating the assertion.
import type * as Acp from "@agentclientprotocol/sdk" with { "resolution-mode": "import" };

export type ContentBlock = Acp.ContentBlock;
export type ToolCall = Acp.ToolCall;
export type ToolCallUpdate = Acp.ToolCallUpdate;
export type ToolCallContent = Acp.ToolCallContent;
export type SessionUpdate = Acp.SessionUpdate;
export type SessionNotification = Acp.SessionNotification;
export type PromptResponse = Acp.PromptResponse;
export type StopReason = Acp.StopReason;
export type NewSessionResponse = Acp.NewSessionResponse;
export type PermissionOption = Acp.PermissionOption;
export type RequestPermissionRequest = Acp.RequestPermissionRequest;
export type RequestPermissionResponse = Acp.RequestPermissionResponse;
export type RequestPermissionOutcome = Acp.RequestPermissionOutcome;
export type ClientContext = Acp.ClientContext;
export type ActiveSession = Acp.ActiveSession;
export type ActiveSessionMessage = Acp.ActiveSessionMessage;
export type InitializeResponse = Acp.InitializeResponse;
export type SessionConfigOption = Acp.SessionConfigOption;
export type SessionConfigSelectOption = Acp.SessionConfigSelectOption;
export type SessionConfigSelectGroup = Acp.SessionConfigSelectGroup;
export type AcpSessionInfo = Acp.SessionInfo;
export type ListSessionsResponse = Acp.ListSessionsResponse;
export type LoadSessionResponse = Acp.LoadSessionResponse;
