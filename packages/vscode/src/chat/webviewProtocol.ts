// Wire contract between the extension host (ChatWebviewViewProvider) and the
// bundled webview script (webview-src/chat-entry.tsx). Kept independent of
// packages/beam's types since the two sides compile separately (tsc vs esbuild).

export type WireSession = {
  id: string;
  title: string;
  createdAt: string;
  updatedAt: string;
  lastMessageAt?: string | null;
};

export type WireMessageRole = "system" | "user" | "assistant" | "tool";

export type WireCitation = {
  title?: string;
  source?: string;
  url?: string;
  path?: string;
};

export type WireToolCall = {
  id: string;
  title?: string;
  status: string;
  toolName?: string;
  output?: string;
  error?: string;
  diff?: { path?: string; before?: string; after?: string };
};

// The ACP StopReason for a completed turn -- Phase 2 "turn states". Kept as a
// plain string union (not imported from acp/types) so this module stays
// independent of the ACP SDK's types, per this file's header comment.
export type WireStopReason = "end_turn" | "max_tokens" | "max_turn_requests" | "refusal" | "cancelled";

export type WireMessage = {
  id: string;
  sessionId: string;
  role: WireMessageRole;
  content: string;
  createdAt: string;
  citations?: WireCitation[];
  toolCalls?: WireToolCall[];
  error?: string;
  // Set on the assistant message once the turn ends. "cancelled" is
  // deliberately not surfaced via `error` -- a cancelled turn is not a
  // failure (vscode-implementation-plan.md Phase 2).
  stopReason?: WireStopReason;
};

export type WireCommand = {
  name: string;
  description: string;
  hint?: string;
};

// Phase 3 "real context attachment" (vscode-implementation-plan.md §5):
// structured, visible, removable attachments -- replaces the old
// `pendingContext` TTL side-channel + composer text-stuffing. Each attachment
// becomes a `resource_link` content block (+ a paired text block carrying the
// frozen content, so the model actually sees it rather than needing its own
// read_file tool call) on the ACP wire; see AcpChatClient's buildPromptBlocks.
export type WireAttachmentKind =
  | "file"
  | "symbol"
  | "selection"
  | "active_file"
  | "diagnostics"
  | "git_diff"
  | "image";

export type WireAttachment = {
  // Client-generated (webview) id for chip identity/removal; images and
  // editor-command-originated attachments generate one client-side, host-
  // resolved attachments (file/symbol/selection/active_file) get one from the
  // host so repeated resolutions don't collide.
  id: string;
  kind: WireAttachmentKind;
  // Chip label.
  name: string;
  // Chip subtitle/tooltip -- relative path, line range, etc.
  description?: string;
  // file/symbol/selection/active_file: the resource's URI (resource_link's
  // `uri`/`name` fields come from this + `name` above).
  uri?: string;
  languageId?: string;
  // Frozen text snapshot for every non-image kind -- captured at attach time,
  // not re-read at send time, so "what you attached" is exactly what is sent.
  text?: string;
  // image kind only.
  mimeType?: string;
  data?: string;
};

export type WireFilePick = {
  uri: string;
  name: string;
  description: string;
};

export type WireSymbolPick = {
  uri: string;
  name: string;
  description: string;
  line: number;
};

// A gated call that parked past the fast window and checkpointed server-side
// (agentservice.StopSuspended / localtools.ApprovalParkWindow) -- "walk-away"
// (vscode-implementation-plan.md Phase 5). approvalId is also the durable
// `contenox approvals` ask id, answerable from any process.
export type WireSuspendedApproval = {
  approvalId: string;
  title?: string;
  toolsName?: string;
  toolName?: string;
  policyName?: string;
  policyPath?: string;
  details?: string;
};

export type WireSessionResponse = {
  session?: WireSession;
  messages?: WireMessage[];
  // Set when this session's most recent turn is currently parked on an
  // unanswered approval -- present on both the sendMessage result (the live
  // in-panel case) and getSession (the "reopened while still suspended"
  // case). Absent/null means nothing is parked.
  suspended?: WireSuspendedApproval | null;
};

export type WireTool = {
  id: string;
  label: string;
  mode: string;
  enabled: boolean;
};

export type WireRuntimeSummary = {
  provider?: string;
  model?: string;
  think?: string;
  hitlPolicy?: string;
  connected: boolean;
  configured?: boolean; // from health.configured (has default provider + model)
  status?: string;      // e.g. "ok" | "setup_required"
  // context usage indicator (from engine token events + session effective token_limit / chain budget)
  // size = the controllable session context budget (capped by model if reported >0)
  contextUsed?: number;
  contextSize?: number;
};

export type WireApprovalOption = {
  id: string;
  label: string;
  kind: string;
};

export type WireApprovalRequest = {
  approvalId: string;
  title: string;
  toolsName?: string;
  toolName?: string;
  // Why the call was gated (approvalflow.Meta's policyName/policyPath),
  // rendered instead of just the tool name (Phase 2 "why it was gated").
  policyName?: string;
  policyPath?: string;
  matchedRule?: number;
  // The matched rule's human-readable cause; displaces matchedRule in the
  // rendered reason when present (see ChatSurface.tsx's gateReason).
  matchedRuleDetail?: string;
  details?: string;
  diff?: { path?: string; before?: string; after?: string };
  options: WireApprovalOption[];
};

export type ChatWebviewToHostMessage =
  | { type: "ready" }
  | { type: "listSessions"; requestId: string }
  | { type: "createSession"; requestId: string; title: string }
  | { type: "getSession"; requestId: string; id: string }
  | { type: "renameSession"; requestId: string; id: string; title: string }
  | { type: "deleteSession"; requestId: string; id: string }
  | { type: "sendMessage"; requestId: string; id: string; content: string; attachments: WireAttachment[] }
  | { type: "cancelTurn"; id: string }
  | { type: "listTools"; requestId: string }
  | { type: "approvalResponse"; requestId: string; optionId?: string }
  | { type: "openDiff"; call: WireToolCall }
  | { type: "confirmDelete"; requestId: string; id: string; title: string }
  | { type: "promptRename"; requestId: string; id: string; title: string }
  | { type: "getRuntimeSummary"; requestId: string }
  | { type: "openRuntimeSettings" }
  // Phase 3 attachment pickers/one-click attachments (§5): all resolved
  // host-side since only the extension host has vscode.workspace/editor API.
  | { type: "searchFiles"; requestId: string; query: string }
  | { type: "searchSymbols"; requestId: string; query: string }
  | { type: "attachFile"; requestId: string; uri: string }
  | { type: "attachSymbol"; requestId: string; uri: string; line: number; name: string }
  | { type: "attachSelection"; requestId: string }
  | { type: "attachActiveFile"; requestId: string }
  // Phase 4 "code out of the panel" (§5): pure client-side VS Code API calls
  // on a code block the panel already renders, no protocol work.
  | { type: "insertAtCursor"; requestId: string; code: string }
  | { type: "applyCodeBlock"; requestId: string; code: string; language?: string; hintPath?: string }
  // Phase 5 "walk-away": re-check a parked session without forcing a full
  // session/load replay (session/resume rebinds only) -- resolves to whatever
  // is now durably true, catching up the transcript if a verdict landed out
  // of band (e.g. `contenox approvals respond`) while nobody was watching.
  | { type: "checkSuspended"; requestId: string; id: string; approvalId: string }
  // Answers a parked approval that has no live in-panel RPC to resolve (the
  // process that asked is gone -- VS Code was closed and reopened), by
  // shelling out to `contenox approvals respond` the same way the CLI would.
  | { type: "respondSuspendedApproval"; requestId: string; id: string; approvalId: string; verdict: "approve" | "deny" };

export type ChatHostToWebviewMessage =
  | { type: "result"; requestId: string; ok: true; value: unknown }
  | { type: "result"; requestId: string; ok: false; error: string }
  | { type: "delta"; requestId: string; content?: string; thinking?: string }
  | { type: "toolCall"; requestId: string; call: WireToolCall }
  | { type: "approvalRequest"; requestId: string; request: WireApprovalRequest }
  // Attachments carried directly on the message (Phase 3): editor commands
  // (Ask/Fix Selection, Review Changes, ...) attach structured context this
  // way now, instead of the old `pendingContext` TTL side-channel.
  | { type: "composerAction"; nonce: string; content: string; submit: boolean; attachments?: WireAttachment[] }
  | { type: "selectSession"; id: string }
  | { type: "runtimeConfig"; summary: WireRuntimeSummary }
  // usage_update, re-pushed live during a turn (Phase 2 "context/token usage").
  | { type: "usage"; used: number; size: number; cost?: { amount: number; currency: string } }
  // available_commands_update, full-replacement slash-command menu (Phase 2
  // "slash commands").
  | { type: "commands"; commands: WireCommand[] }
  // Phase 5 "walk-away": pushed unsolicited when a parked session's
  // approval resolves out of band (e.g. `contenox approvals respond` from a
  // terminal) while the panel is open watching it -- there is no live
  // sendMessage request left to carry this, since that one already resolved
  // at the moment the run suspended.
  | { type: "sessionCaughtUp"; id: string; session?: WireSession; messages?: WireMessage[] };
