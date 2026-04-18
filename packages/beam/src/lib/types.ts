export type ModelDescriptor = {
  id?: string;
  name: string;
  sourceUrl: string;
  sizeBytes: number;
  curated: boolean;
};

export type ModelRegistryEntry = {
  id: string;
  name: string;
  sourceUrl: string;
  sizeBytes: number;
  createdAt?: string;
  updatedAt?: string;
};

export type Backend = {
  id: string;
  name: string;
  baseUrl: string;
  type: string;
  /** Runtime-observed model names for this backend. */
  models: string[];
  pulledModels: ObservedModel[];
  error: string;
  createdAt?: string;
  updatedAt?: string;
};

/** GET /api/state — runtime-observed backend state (same shape as statetype.BackendRuntimeState JSON). */
export type BackendRuntimeState = {
  id: string;
  name: string;
  models: string[];
  pulledModels: ObservedModel[];
  backend: Backend;
  error?: string;
};

export type ObservedModel = {
  name?: string;
  model: string;
  modifiedAt?: string;
  size?: number;
  digest?: string;
  contextLength: number;
  canChat: boolean;
  canEmbed: boolean;
  canPrompt: boolean;
  canStream: boolean;
};

export type LayoutDirection = 'horizontal' | 'vertical';

export type StateResponse = {
  response: string;
  state: CapturedStateUnit[];
  inputTokenCount: number;
  outputTokenCount: number;
  error?: string;
};

/** POST /api/chats/:id/chat — optional body fields (server resolves chain from mode when chainId omitted). */
export type ChatModeId = 'chat' | 'prompt' | 'plan' | 'build';

export type ChatContextArtifact = {
  kind: string;
  /** JSON value serialized per artifact */
  payload?: unknown;
};

export type ChatContextPayload = {
  artifacts?: ChatContextArtifact[];
};

export type TaskEventKind =
  | 'chain_started'
  | 'step_started'
  | 'step_chunk'
  | 'step_completed'
  | 'step_failed'
  | 'chain_completed'
  | 'chain_failed'
  | 'approval_requested';

export type TaskEvent = {
  kind: TaskEventKind;
  timestamp: string;
  request_id?: string;
  chain_id?: string;
  task_id?: string;
  task_handler?: string;
  retry?: number;
  model_name?: string;
  provider_type?: string;
  backend_id?: string;
  output_type?: string;
  transition?: string;
  content?: string;
  thinking?: string;
  error?: string;
  /**
   * Widget hints emitted by hooks during the just-completed step (Phase 5
   * of the Beam canvas-vision plan). Mirrors taskengine.WidgetHint on the
   * Go side. The shape matches ChatContextArtifact so the same artifact →
   * inline-attachment mapping handles both directions of state flow.
   */
  attachments?: Array<{ kind: string; payload?: unknown }>;

  // Approval fields — only present on approval_requested events.
  approval_id?: string;
  hook_name?: string;
  tool_name?: string;
  approval_args?: Record<string, unknown>;
  approval_diff?: string;
};

export type SetupIssue = {
  code: string;
  severity: string;
  category?: string;
  message: string;
  fixPath?: string;
  cliCommand?: string;
};

export type SetupBackendCheck = {
  id: string;
  name: string;
  type: string;
  baseUrl: string;
  status: string;
  reachable: boolean;
  defaultProvider: boolean;
  modelCount: number;
  chatModelCount: number;
  chatModels?: string[];
  error?: string;
  hint?: string;
};

export type SetupStatus = {
  defaultModel: string;
  defaultProvider: string;
  /** VFS-relative or absolute chain path (same KV as `contenox config set default-chain`). */
  defaultChain: string;
  /** Active HITL policy filename (same KV as `contenox config set hitl-policy-name`). */
  hitlPolicyName: string;
  backendCount: number;
  reachableBackendCount: number;
  issues: SetupIssue[];
  backendChecks: SetupBackendCheck[];
};

export type CLIConfigUpdateResponse = {
  defaultModel: string;
  defaultProvider: string;
  defaultChain: string;
  hitlPolicyName: string;
};

export type HITLCondition = {
  key: string;
  op: 'eq' | 'glob';
  value: string;
};

export type HITLRule = {
  hook: string;
  tool: string;
  when?: HITLCondition[];
  action: 'allow' | 'approve' | 'deny';
  timeout_s?: number;
  on_timeout?: 'deny' | 'approve';
};

export type HITLPolicy = {
  default_action?: 'allow' | 'approve' | 'deny';
  rules: HITLRule[];
};

/** Mirrors planstore + planapi JSON (snake_case). */
export type PlanStatus = 'active' | 'completed' | 'archived';

export type StepStatus = 'pending' | 'running' | 'completed' | 'failed' | 'skipped';

export type Plan = {
  id: string;
  name: string;
  goal: string;
  status: PlanStatus;
  session_id: string;
  /** Full plancompile.Compile output JSON when cached (server). */
  compiled_chain_json?: string;
  compiled_chain_id?: string;
  compile_executor_chain_id?: string;
  created_at: string;
  updated_at: string;
};

export type PlanStep = {
  id: string;
  plan_id: string;
  ordinal: number;
  description: string;
  status: StepStatus;
  execution_result: string;
  executed_at: string;
};

export type NewPlanResponse = {
  plan: Plan;
  steps: PlanStep[];
  markdown: string;
};

export type ActivePlanResponse = {
  plan: Plan;
  steps: PlanStep[];
};

export type NextStepResponse = {
  result: string;
  markdown: string;
};

export type ReplanResponse = {
  steps: PlanStep[];
  markdown: string;
};

export type PlanMarkdownResponse = {
  markdown: string;
};

export type CleanPlansResponse = {
  removed: number;
};

/** POST /api/plans/compile */
export type CompilePlanResponse = {
  goal: string;
  steps: string[];
  chain: ChainDefinition;
  path?: string;
};

/** POST /api/plans/active/run-compiled */
export type RunCompiledActiveResponse = {
  goal: string;
  steps: string[];
  chain?: ChainDefinition;
  path?: string;
  output: unknown;
  output_type: string;
  state: CapturedStateUnit[];
};

export type SearchResult = {
  id: string;
  resourceType: string;
  distance: number;
  fileMeta: FileResponse;
};

export type StatusResponse = {
  configured: boolean;
  provider: string;
};

export type CloudProviderType = 'ollama' | 'openai' | 'gemini';

export type SearchResponse = {
  results: SearchResult[];
};

export type ModelJob = {
  url: string;
  model: string;
};

export type Job = {
  id: string;
  taskType: string;
  modelJob: ModelJob | undefined;
  scheduledFor: number;
  validUntil: number;
  createdAt: Date;
};

export type ChatSession = {
  id: string;
  startedAt: string;
  model: string;
  lastMessage?: ChatMessage;
};

export type CapturedStateUnit = {
  taskID: string;
  taskType: string;
  inputType: string;
  outputType: string;
  transition: string;
  duration: number;
  error: ErrorState;
};

export type ErrorState = {
  error: string | null;
};

export type ToolCallFunction = {
  name: string;
  arguments: string;
};

export type ToolCallEntry = {
  id: string;
  type?: string;
  function: ToolCallFunction;
};

export type ChatMessage = {
  id?: string;
  role: 'user' | 'assistant' | 'system' | 'tool';
  content: string;
  sentAt: string;
  isUser: boolean;
  isLatest: boolean;
  state?: CapturedStateUnit[];
  error?: string;
  /** Assistant message still receiving task-event stream (Beam live row). */
  streaming?: boolean;
  /** Inline attachments rendered alongside this message in the chat thread. */
  attachments?: InlineAttachment[];
  /** Tool calls requested by this assistant message; used to label tool-result messages. */
  callTools?: ToolCallEntry[];
  /** For role=tool messages: links this result back to the assistant's callTools entry. */
  toolCallId?: string;
};

/**
 * Inline attachments are typed renderer cards that appear adjacent to a
 * message in the chat thread. They mirror artifact kinds 1:1 (see
 * packages/beam/src/lib/artifacts/types.ts) but are oriented toward
 * presentation (collapsibility, syntax highlighting, links) rather than the
 * LLM-context shape.
 */
export type InlineAttachment =
  | { kind: 'file_view'; path: string; text: string; truncated?: boolean }
  | {
      kind: 'terminal_excerpt';
      output: string;
      command?: string;
      sessionId?: string;
      capturedAt?: string;
    }
  | {
      kind: 'plan_summary';
      planId: string;
      ordinal: number;
      description: string;
      status: string;
      summary?: string;
      failureClass?: string;
    }
  | { kind: 'dag'; chainJSON: string; description?: string }
  | { kind: 'state_unit'; name: string; data?: unknown };

export type QueueItem = {
  url: string;
  model: string;
  status: QueueProgressStatus;
};

export type QueueProgressStatus = {
  total: number;
  completed: number;
  status: string;
};

export type Model = {
  id: string;
  model: string;
  contextLength: number;
  canChat: boolean;
  canEmbed: boolean;
  canPrompt: boolean;
  canStream: boolean;
  createdAt?: string;
  updatedAt?: string;
};

export type Pool = {
  id: string;
  name: string;
  purposeType: string;
  createdAt?: string;
  updatedAt?: string;
};

export type AuthResponse = {
  user: User;
};

export type LocalHook = {
  name: string;
  description: string;
  type: string;
  /** Origin of this hook in the merged discovery list (from API). */
  source?: 'builtin' | 'mcp' | 'remote';
  tools: Tool[];
  /** Present when the server could not load tools (e.g. unreachable MCP). */
  unavailableReason?: string;
};

/** Persisted MCP server config; matches runtimetypes.MCPServer JSON. */
export type MCPServer = {
  id: string;
  name: string;
  transport: 'stdio' | 'sse' | 'http' | string;
  command?: string;
  args?: string[];
  url?: string;
  authType?: string;
  authToken?: string;
  authEnvKey?: string;
  connectTimeoutSeconds: number;
  headers?: Record<string, string>;
  injectParams?: Record<string, string>;
  createdAt?: string;
  updatedAt?: string;
};

export type Tool = {
  type: string;
  function: {
    name: string;
    description: string;
    parameters: Record<string, unknown>;
  };
};

export type User = {
  id: string;
  friendlyName: string;
  email: string;
  subject: string;
  password: string;
  createdAt?: string;
  updatedAt?: string;
};

export type DownloadStatus = {
  status: string;
  digest?: string;
  total?: number;
  completed?: number;
  model: string;
  baseUrl: string;
};

export type AccessEntry = {
  id: string;
  identity: string;
  resource: string;
  resourceType: string;
  permission: string;
  createdAt?: string;
  updatedAt?: string;
  identityDetails?: IdentityDetails;
  fileDetails?: filesDetails;
};

export type filesDetails = {
  id: string;
  path: string;
  type: string;
};

export type IdentityDetails = {
  id: string;
  friendlyName: string;
  email: string;
  subject: string;
};

export type UpdateUserRequest = {
  email?: string;
  subject?: string;
  friendlyName?: string;
  password?: string;
};

export type UpdateAccessEntryRequest = {
  identity?: string;
  resource?: string;
  permission?: string;
};

export type FolderResponse = {
  id: string;
  path: string;
  createdAt?: string;
  updatedAt?: string;
};

export type PathUpdateRequest = {
  path: string;
};

/** VFS file row from GET /api/files and related routes. */
export interface FileResponse {
  id: string;
  path: string;
  name?: string;
  contentType: string;
  size: number;
  createdAt?: string;
  updatedAt?: string;
  /** Present for directory entries in directory listings. */
  isDirectory?: boolean;
}

// Beam auth is backed by /api/ui/login and /api/ui/me, which currently return
// a lightweight server auth payload rather than the full User shape.
export type AuthenticatedUser = {
  id: string;
  subject: string;
  email: string;
  friendlyName: string;
  username: string;
  expiresAt?: string;
};

export type PendingJob = {
  id: string;
  taskType: string;
  operation: string;
  subject: string;
  entityId: string;
  scheduledFor: string;
  validUntil: string;
  retryCount: number;
  createdAt: string;
};

export type InProgressJob = PendingJob & {
  leaser: string;
  leaseExpiration: string;
};
export type Exec = {
  prompt: string;
};

export type ExecResp = {
  id: string;
  response: string;
};

export type TaskExecutionRequest = {
  input: unknown;
  inputType: string;
  chain: ChainDefinition;
  templateVars?: Record<string, string>;
};

export type TaskExecutionResponse = {
  output: unknown;
  outputType: string;
  state: CapturedStateUnit[];
};

export interface HookCall {
  name: string;
  tool_name?: string;
  args?: Record<string, string>;
}

export interface TransitionBranch {
  operator?: OperatorTerm;
  when: string;
  goto: string;
  compose?: BranchCompose;
}

export interface ChainTask {
  id: string;
  description: string;
  handler: TaskHandler;
  system_instruction?: string;
  valid_conditions?: Record<string, boolean>;
  execute_config?: ExecuteConfig;
  hook?: HookCall;
  print?: string;
  prompt_template: string;
  output_template?: string;
  input_var?: string;
  transition: TaskTransition;
  timeout?: string;
  retry_on_failure?: number;
}

export type ComposeStrategy = 'override' | 'merge_chat_histories' | 'append_string_to_chat_history';

export type OperatorTerm =
  | 'equals'
  | 'contains'
  | 'starts_with'
  | 'ends_with'
  | 'gt'
  | 'lt'
  | 'in_range'
  | 'default';

// Keep exactly ONE TransitionBranch definition (delete any duplicates)
export interface TransitionBranch {
  operator?: OperatorTerm;
  when: string;
  goto: string;
  compose?: BranchCompose;
}

// ✅ Add this: the missing TaskTransition type
export interface TaskTransition {
  /** Task id to go to on failure, or 'end' (omit/undefined for none). */
  on_failure?: string;
  /** Conditional branches; use goto: 'end' to finish. */
  branches: TransitionBranch[];
}

// Forms use the same shape; alias for clarity in UI layer
export type FormTransition = TaskTransition;

// Keep ChainTask referring to TaskTransition
export interface ChainTask {
  id: string;
  description: string;
  handler: TaskHandler;
  system_instruction?: string;
  valid_conditions?: Record<string, boolean>;
  execute_config?: ExecuteConfig;
  hook?: HookCall;
  print?: string;
  prompt_template: string;
  output_template?: string;
  input_var?: string;
  transition: TaskTransition; // <- now resolvable
  timeout?: string;
  retry_on_failure?: number;
}

// FormTask keeps partial but requires keys we edit frequently
export type FormTask = Partial<ChainTask> & {
  id: string;
  handler: TaskHandler;
  prompt_template: string;
  transition: TaskTransition;
};

// RetryPolicy mirrors taskengine/llmretry.RetryPolicy. Durations are expressed
// as Go duration strings ("500ms", "30s", "2m"); the backend also accepts
// nanoseconds as numbers but this editor writes strings for readability.
export interface RetryPolicy {
  max_attempts?: number;
  initial_backoff?: string;
  max_backoff?: string;
  jitter?: number;
  rate_limit_min_wait?: string;
  fallback_model_id?: string;
  fallback_after?: number;
}

// CompactPolicy mirrors taskengine/compact.Policy. Controls mid-run conversation
// compaction on the executor's chat_completion task.
export interface CompactPolicy {
  trigger_fraction?: number;
  keep_recent?: number;
  model?: string;
  provider?: string;
  max_failures?: number;
  min_replaced_messages?: number;
}

export interface ExecuteConfig {
  model?: string;
  models?: string[];
  provider?: string;
  providers?: string[];
  temperature?: number;
  hooks?: string[];
  hide_tools?: string[];
  pass_clients_tools?: boolean;
  // hook_policies maps hook name → (policy key → value). Free-form; edited as
  // a collapsible JSON block in the chain editor when present.
  hook_policies?: Record<string, Record<string, string>>;
  // think: "", "low", "medium", "high", or "true" / "false" — provider-gated.
  think?: string;
  // shift: allow the context window to slide on overflow instead of erroring.
  shift?: boolean;
  // retry_policy: classified retry/backoff + optional fallback model.
  // See taskengine/llmretry.RetryPolicy.
  retry_policy?: RetryPolicy;
  // compact_policy: mid-run conversation compaction.
  // See taskengine/compact.Policy.
  compact_policy?: CompactPolicy;
}

export interface ChainDefinition {
  id: string;
  description: string;
  tasks: ChainTask[];
  token_limit?: number;
  debug?: boolean;
}

export type ActivityLog = {
  id: string;
  operation: string;
  subject: string;
  start: string;
  end?: string;
  error?: string;
  entityID?: string;
  entityData?: undefined;
  durationMS?: number;
  metadata?: Record<string, string>;
  requestID?: string;
};

export type ActivityLogsResponse = ActivityLog[];

export type TrackedRequest = {
  id: string;
};

export type ActivityOperation = {
  operation: string;
  subject: string;
};

export type TrackedEvent = {
  id: string;
  operation: string;
  subject: string;
  start: string;
  end?: string;
  error?: string;
  entityID?: string;
  entityData?: unknown;
  durationMS?: number;
  metadata?: Record<string, string>;
  requestID?: string;
};

export type Operation = {
  operation: string;
  subject: string;
};

export type TrackedRequestsResponse = TrackedRequest[];
export type ActivityOperationsResponse = ActivityOperation[];

export type Alert = {
  id: string;
  requestID: string;
  metadata: unknown;
  message: string;
  timestamp: string;
};

export type ActivityAlertsResponse = Alert[];

export interface GitHubRepo {
  id: string;
  userID: string;
  botUserName: string;
  owner: string;
  repoName: string;
  accessToken: string;
  createdAt: string;
  updatedAt: string;
}

export interface PullRequest {
  id: number;
  number: number;
  title: string;
  state: string;
  url: string;
  createdAt: string;
  updatedAt: string;
  authorLogin: string;
}

export type TelegramFrontend = {
  id: string;
  userID: string;
  chatChain: string;
  description: string;
  botToken: string;
  syncInterval: number;
  status: string;
  lastOffset: number;
  lastHeartbeat?: string;
  lastError: string;
  createdAt?: string;
  updatedAt?: string;
};

export type Bot = {
  id: string;
  name: string;
  botType: string;
  jobType: string;
  taskChainID: string;
  createdAt: string;
  updatedAt: string;
};

export type InternalEvent = {
  id: string;
  nid: number;
  created_at: string;
  event_type: string;
  event_source: string;
  aggregate_id: string;
  aggregate_type: string;
  version: number;
  data: Record<string, unknown>;
  metadata: Record<string, unknown>;
};

export type RawEvent = {
  id: string;
  nid: number;
  received_at: string;
  path: string;
  headers: Record<string, string>;
  payload: Record<string, unknown>;
};

export type MappingConfig = {
  path: string;
  eventType: string;
  eventSource: string;
  aggregateType: string;
  aggregateIDField: string;
  aggregateTypeField: string;
  eventTypeField: string;
  eventSourceField: string;
  eventIDField: string;
  version: number;
  metadataMapping: Record<string, string>;
};

export type EventStreamMessage = {
  id: string;
  event_type: string;
  aggregate_type: string;
  aggregate_id: string;
  version: number;
  data: Record<string, unknown>;
  created_at: string;
};

// Executor and Task Types
export type TaskRequest = {
  prompt: string;
  modelName: string;
  modelProvider: string;
};

export type TaskResponse = {
  id: string;
  response: string;
};

export type DataType = 'string' | 'json';

export type ExecutionHistory = {
  taskID: string;
  taskType: string;
  inputType: string;
  outputType: string;
  transition: string;
  duration: number;
  error?: string;
}[];

// Pagination Types
export type PaginationParams = {
  limit?: number;
  cursor?: string;
};

export type PaginatedResponse<T> = {
  data: T[];
  nextCursor?: string;
  hasMore: boolean;
};

// Validation Error Types
export type ValidationError = {
  field: string;
  message: string;
  code: string;
};

export type ApiError = {
  message: string;
  code: string;
  details?: ValidationError[];
};

// Webhook and Integration Types
export type WebhookPayload = {
  headers: Record<string, string>;
  body: Record<string, unknown>;
  query: Record<string, string>;
  method: string;
  path: string;
};

export interface BranchCompose {
  with_var?: string;
  strategy?: string;
}

export type IntegrationConfig = {
  id: string;
  name: string;
  type: 'webhook' | 'api' | 'event';
  config: Record<string, unknown>;
  enabled: boolean;
  createdAt?: string;
  updatedAt?: string;
};

// System and Health Types
export type SystemHealth = {
  status: 'healthy' | 'degraded' | 'unhealthy';
  components: {
    database: HealthStatus;
    cache: HealthStatus;
    executor: HealthStatus;
    eventBus: HealthStatus;
  };
  timestamp: string;
};

export type HealthStatus = {
  status: 'up' | 'down' | 'degraded';
  latency?: number;
  error?: string;
  lastChecked: string;
};

export interface BackendApiError {
  error: {
    message: string;
    type?: string;
    code?: string;
    param?: string;
  };
}

export type RemoteHook = {
  id: string;
  name: string;
  endpointUrl: string;
  timeoutMs: number;
  headers?: Record<string, string>;
  properties: InjectionArg;
  createdAt?: string;
  updatedAt?: string;
};

export type InjectionArg = {
  name: string;
  value: unknown;
  in: 'path' | 'query' | 'body';
};

// For listing with pagination
export type RemoteHookListResponse = {
  hooks: RemoteHook[];
  nextCursor?: string; // RFC3339Nano timestamp
};

export interface DragResult {
  draggableId: string;
  source: {
    index: number;
    droppableId: string;
  };
  destination: {
    index: number;
    droppableId: string;
  } | null;
}

export interface DroppableProvided {
  innerRef: (element: HTMLElement | null) => void;
  droppableProps: {
    [key: string]: unknown;
  };
  placeholder?: React.ReactNode;
}

export interface DraggableProvided {
  innerRef: (element: HTMLElement | null) => void;
  draggableProps: {
    [key: string]: unknown;
    style?: React.CSSProperties;
  };
  dragHandleProps: {
    [key: string]: unknown;
  } | null;
}

export type TaskHandler =
  | 'prompt_to_condition'
  | 'prompt_to_int'
  | 'prompt_to_float'
  | 'prompt_to_range'
  | 'prompt_to_string'
  | 'text_to_embedding'
  | 'raise_error'
  | 'chat_completion'
  | 'execute_tool_calls'
  | 'parse_command'
  | 'parse_key_value'
  | 'convert_to_openai_chat_response'
  | 'noop'
  | 'hook';

export const HandleConditionKey: TaskHandler = 'prompt_to_condition';
export const HandleParseNumber: TaskHandler = 'prompt_to_int';
export const HandleParseScore: TaskHandler = 'prompt_to_float';
export const HandleParseRange: TaskHandler = 'prompt_to_range';
export const HandlePromptToString: TaskHandler = 'prompt_to_string';
export const HandleEmbedding: TaskHandler = 'text_to_embedding';
export const HandleRaiseError: TaskHandler = 'raise_error';
export const HandleChatCompletion: TaskHandler = 'chat_completion';
export const HandleExecuteToolCalls: TaskHandler = 'execute_tool_calls';
export const HandleParseTransition: TaskHandler = 'parse_command';
export const HandleParseKeyValue: TaskHandler = 'parse_key_value';
export const HandleConvertToOpenAIChatResponse: TaskHandler = 'convert_to_openai_chat_response';
export const HandleNoop: TaskHandler = 'noop';
export const HandleHook: TaskHandler = 'hook';

// ── Terminal ──────────────────────────────────────────────────────────

export type TerminalSessionCreate = {
  id: string;
  wsPath: string;
};

export type TerminalSession = {
  id: string;
  principal: string;
  cwd: string;
  shell: string;
  cols: number;
  rows: number;
  status: string;
  nodeInstanceId: string;
  createdAt: string;
  updatedAt: string;
};

