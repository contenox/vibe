export const protocolVersion = 1;

export type JsonRpcID = number | string | null;

export interface JsonRpcRequest {
  jsonrpc: "2.0";
  id: JsonRpcID;
  method: string;
  params?: unknown;
}

export interface JsonRpcResponse<T = unknown> {
  jsonrpc: "2.0";
  id?: JsonRpcID;
  result?: T;
  error?: JsonRpcError;
}

export interface JsonRpcNotification<T = unknown> {
  jsonrpc: "2.0";
  method: string;
  params?: T;
}

export interface JsonRpcError {
  code: number;
  message: string;
  data?: unknown;
}

export interface InitializeParams {
  clientInfo?: ClientInfo;
  workspace?: string;
  workspacePath?: string;
}

export interface ClientInfo {
  name?: string;
  version?: string;
}

export interface InitializeResult {
  protocolVersion: number;
  serverVersion: string;
  stateDir: string;
  workspaceId: string;
  workspaceMode: string;
  capabilities: BridgeCapabilities;
  config: ConfigSnapshot;
}

export interface BridgeCapabilities {
  config: boolean;
  providers: boolean;
  models: boolean;
  chat: boolean;
  autocomplete: boolean;
  hitl: boolean;
  sessionList: boolean;
  commands?: boolean;
  mcp?: boolean;
}

export interface ConfigSnapshot {
  defaultProvider?: string;
  defaultModel?: string;
  defaultAltProvider?: string;
  defaultAltModel?: string;
  defaultAutocompleteProvider?: string;
  defaultAutocompleteModel?: string;
  defaultMaxTokens?: string;
  defaultThink?: string;
  defaultChain?: string;
  hitlPolicyName?: string;
  scopes?: Record<string, string>;
}

export interface SetConfigParams {
  defaultProvider?: string;
  defaultModel?: string;
  defaultAltProvider?: string;
  defaultAltModel?: string;
  defaultAutocompleteProvider?: string;
  defaultAutocompleteModel?: string;
  defaultMaxTokens?: string;
  defaultThink?: string;
  defaultChain?: string;
  hitlPolicyName?: string;
}

export interface HealthResult {
  status: string;
  configured: boolean;
  defaultProvider?: string;
  defaultModel?: string;
  config: ConfigSnapshot;
  configuredBackends: number;
}

export interface ListProvidersResult {
  providers: ProviderInfo[];
}

export interface ProviderInfo {
  provider: string;
  configured: boolean;
  backendId?: string;
  backendName?: string;
  baseUrl?: string;
  requiresBaseUrl: boolean;
  requiresSecretConfig: boolean;
  recommendedApiKeyEnv?: string;
  defaultProvider?: string;
  defaultModel?: string;
}

export interface ListModelsParams {
  provider?: string;
}

export interface ListModelsResult {
  models: ModelInfo[];
}

export interface ModelInfo {
  id: string;
  provider?: string;
  name: string;
  displayName: string;
  contextLength?: number;
  capabilities: Record<string, boolean>;
  source: "observed" | "config" | string;
}

export interface ListHitlPoliciesResult {
  policies: string[];
  policyDir?: string;
  activePolicyName?: string;
  activePolicyPath?: string;
  policyFiles?: HitlPolicyInfo[];
}

export interface HitlPolicyInfo {
  name: string;
  path?: string;
  active?: boolean;
}

export interface SlashCommandInfo {
  name: string;
  description: string;
  hint?: string;
}

export interface ListCommandsResult {
  commands: SlashCommandInfo[];
}

export interface ListMCPServersResult {
  servers: MCPServerInfo[];
}

export interface MCPServerInfo {
  id?: string;
  name: string;
  transport: "stdio" | "http" | "sse" | string;
  command?: string;
  args?: string[];
  url?: string;
  authType?: string;
  authEnvKey?: string;
  headers?: Record<string, string>;
  connectTimeoutSeconds?: number;
}

export interface SessionInfo {
  id: string;
  name?: string;
  messageCount: number;
  isActive: boolean;
  updatedAt?: string;
}

export interface SessionMessage {
  id?: string;
  role: "system" | "user" | "assistant" | "tool" | string;
  content?: string;
  thinking?: string;
  toolCallId?: string;
  toolCalls?: SessionToolCall[];
  timestamp?: string;
}

export interface SessionToolCall {
  id?: string;
  name?: string;
  arguments?: Record<string, unknown>;
  rawArgs?: string;
}

export interface SessionResult {
  session: SessionInfo;
  messages: SessionMessage[];
}

export interface SessionListResult {
  sessions: SessionInfo[];
}

export interface SessionCreateParams {
  name?: string;
}

export interface SessionLoadParams {
  sessionId?: string;
  name?: string;
}

export interface SessionReadParams {
  sessionId?: string;
  name?: string;
}

export interface SessionDeleteParams {
  sessionId?: string;
  name?: string;
}

export interface SessionDeleteResult {
  deleted: boolean;
  sessionId?: string;
  wasActive: boolean;
}

export interface EditorContextAttachment {
  kind: string;
  uri?: string;
  languageId?: string;
  content: string;
}

export interface ChatSendParams {
  sessionId?: string;
  input: string;
  context?: EditorContextAttachment[];
  vars?: Record<string, string>;
}

export interface ChatSendResult {
  sessionId: string;
  turnId: string;
  title?: string;
}

export interface ChatCancelParams {
  turnId: string;
}

export interface ChatCancelResult {
  cancelled: boolean;
}

export interface AutocompleteParams {
  prefix: string;
  suffix?: string;
  languageId?: string;
  uri?: string;
  provider?: string;
  model?: string;
  maxTokens?: number;
}

export interface AutocompleteResult {
  completion: string;
}

export interface ChatDeltaEvent {
  sessionId: string;
  turnId: string;
  content?: string;
  thinking?: string;
}

export interface ChatLifecycleEvent {
  sessionId: string;
  turnId: string;
  stopReason?: string;
  error?: string;
  messages?: SessionMessage[];
}

export interface ToolCallEvent {
  sessionId: string;
  turnId: string;
  toolCallId?: string;
  title?: string;
  status: string;
  toolName?: string;
  taskId?: string;
  input?: Record<string, unknown>;
  output?: string;
  error?: string;
  diffPath?: string;
  diffOld?: string;
  diffNew?: string;
}

export interface HitlDecisionEvent {
  sessionId: string;
  turnId: string;
  toolsName?: string;
  toolName?: string;
  action: string;
  reason?: string;
  policyName?: string;
  policyPath?: string;
  argsSummary?: string;
  matchedRule?: number;
  timeoutS?: number;
  approvalRequested: boolean;
}

export interface ApprovalRequestedEvent {
  approvalId: string;
  toolsName?: string;
  toolName?: string;
  title: string;
  policyName?: string;
  policyPath?: string;
  args?: Record<string, unknown>;
  details?: string;
  diff?: string;
  diffOld?: string;
  diffNew?: string;
  options: ApprovalOption[];
}

export interface ApprovalOption {
  id: string;
  label: string;
  kind: string;
}

export interface RequestPermissionParams {
  sessionId: string;
  toolCall: PermissionToolCall;
  options: PermissionOption[];
  _meta?: PermissionMeta;
}

export interface PermissionToolCall {
  toolCallId: string;
  title?: string;
  kind?: string;
  status?: string;
  content?: ToolCallContent[];
  locations?: ToolCallLocation[];
  rawInput?: unknown;
  rawOutput?: unknown;
  _meta?: PermissionMeta;
}

export interface ToolCallContent {
  type: "content" | "diff" | "terminal" | string;
  content?: {
    type?: string;
    text?: string;
  };
  path?: string;
  oldText?: string;
  newText?: string;
  terminalId?: string;
}

export interface ToolCallLocation {
  path: string;
  line?: number;
}

export interface PermissionOption {
  optionId: string;
  name: string;
  kind: "allow_once" | "allow_always" | "reject_once" | "reject_always" | string;
}

export interface PermissionMeta {
  toolsName?: string;
  toolName?: string;
  policyName?: string;
  policyPath?: string;
  diff?: string;
  diffOld?: string;
  diffNew?: string;
}

export interface RequestPermissionResponse {
  outcome: PermissionOutcome;
}

export type PermissionOutcome =
  | {
      outcome: "selected";
      optionId: string;
    }
  | {
      outcome: "cancelled";
    };
