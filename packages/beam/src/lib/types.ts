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
  | 'chain_failed';

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
  backendCount: number;
  reachableBackendCount: number;
  issues: SetupIssue[];
  backendChecks: SetupBackendCheck[];
};

export type CLIConfigUpdateResponse = {
  defaultModel: string;
  defaultProvider: string;
  defaultChain: string;
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
};

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

export interface ExecuteConfig {
  model?: string;
  models?: string[];
  provider?: string;
  providers?: string[];
  temperature?: number;
  hooks?: string[];
  hide_tools?: string[];
  pass_clients_tools?: boolean;
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

export type FunctionScriptType = 'goja';
export type FunctionType = 'function';

export type Listener = {
  type: string;
};

export type Function = {
  name: string;
  description?: string;
  scriptType: FunctionScriptType;
  script: string;
  createdAt?: string;
  updatedAt?: string;
};

export type EventTrigger = {
  name: string;
  description?: string;
  listenFor: Listener;
  type: FunctionType;
  function: string;
  createdAt?: string;
  updatedAt?: string;
};

export type FunctionsListResponse = {
  functions: Function[];
  nextCursor?: string;
};

export type EventTriggersListResponse = {
  triggers: EventTrigger[];
  nextCursor?: string;
};

export type CreateFunctionRequest = Omit<Function, 'createdAt' | 'updatedAt'>;
export type UpdateFunctionRequest = Partial<Omit<Function, 'createdAt' | 'updatedAt'>>;
export type CreateEventTriggerRequest = Omit<EventTrigger, 'createdAt' | 'updatedAt'>;
export type UpdateEventTriggerRequest = Partial<Omit<EventTrigger, 'createdAt' | 'updatedAt'>>;

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

// Function Execution Types
export type FunctionExecutionResult = {
  success: boolean;
  result?: Record<string, unknown>;
  error?: string;
};

export type BuiltInFunctionCall = {
  name: 'sendEvent' | 'callTaskChain' | 'executeTask' | 'executeTaskChain';
  args: Record<string, unknown>;
};

// Sync and Cache Types
export type SyncResponse = {
  success: boolean;
  message: string;
  functionsSynced?: number;
  triggersSynced?: number;
  lastSync?: string;
};

export type CacheStatus = {
  functions: {
    count: number;
    lastSync: string;
  };
  triggers: {
    count: number;
    lastSync: string;
  };
};

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

// Metrics and Monitoring Types
export type FunctionMetrics = {
  functionName: string;
  executions: number;
  successes: number;
  failures: number;
  averageDuration: number;
  lastExecution?: string;
  errorRate: number;
};

export type EventMetrics = {
  eventType: string;
  totalEvents: number;
  triggersExecuted: number;
  averageProcessingTime: number;
  lastProcessed?: string;
};

// Advanced Function Types
export type FunctionEnvironment = {
  variables: Record<string, string>;
  timeout: number;
  memoryLimit: number;
  concurrency: number;
};

export type FunctionDeployment = {
  functionName: string;
  version: string;
  deployedAt: string;
  checksum: string;
  status: 'active' | 'inactive' | 'failed';
  runtime: 'goja';
};

// Event Processing Types
export type EventProcessingResult = {
  eventId: string;
  processedAt: string;
  triggersExecuted: string[];
  results: Record<string, FunctionExecutionResult>;
  duration: number;
  success: boolean;
};

export type EventReplayRequest = {
  eventIds: string[];
  fromDate?: string;
  toDate?: string;
  eventTypes?: string[];
};

// Security and Permission Types
export type FunctionPermission = {
  functionName: string;
  allowedActions: string[];
  allowedEvents: string[];
  environmentVariables: string[];
  maxExecutionTime: number;
};

export type TriggerPermission = {
  triggerName: string;
  allowedEventTypes: string[];
  allowedFunctions: string[];
  maxConcurrentExecutions: number;
};

// Debug and Development Types
export type FunctionDebugInfo = {
  functionName: string;
  compiled: boolean;
  cacheHit: boolean;
  compilationTime?: number;
  sourceHash: string;
  lastCompiled?: string;
};

export type ExecutionContext = {
  requestId: string;
  functionName: string;
  eventId?: string;
  triggerName?: string;
  startTime: string;
  timeout: number;
  environment: Record<string, string>;
};

// Batch Operation Types
export type BatchOperation<T> = {
  operations: T[];
  batchId: string;
  total: number;
  processed: number;
  failed: number;
  status: 'pending' | 'processing' | 'completed' | 'failed';
};

export type BatchFunctionUpdate = {
  functionName: string;
  updates: UpdateFunctionRequest;
};

export type BatchTriggerUpdate = {
  triggerName: string;
  updates: UpdateEventTriggerRequest;
};

// Template and Snippet Types
export type FunctionTemplate = {
  id: string;
  name: string;
  description: string;
  category: string;
  code: string;
  variables: string[];
  tags: string[];
  author: string;
  createdAt: string;
  updatedAt: string;
};

export type CodeSnippet = {
  id: string;
  name: string;
  description: string;
  code: string;
  language: 'javascript' | 'typescript';
  category: string;
  tags: string[];
};

// Import/Export Types
export type FunctionExport = {
  functions: Function[];
  triggers: EventTrigger[];
  metadata: {
    exportedAt: string;
    version: string;
    count: {
      functions: number;
      triggers: number;
    };
  };
};

export type ImportResult = {
  imported: {
    functions: number;
    triggers: number;
  };
  skipped: {
    functions: number;
    triggers: number;
  };
  errors: {
    functionErrors: Record<string, string>;
    triggerErrors: Record<string, string>;
  };
};

// Real-time Monitoring Types
export type LiveExecution = {
  executionId: string;
  functionName: string;
  triggerName?: string;
  eventId?: string;
  status: 'running' | 'completed' | 'failed';
  startTime: string;
  duration?: number;
  progress?: number;
  logs: string[];
};

export type ExecutionLog = {
  id: string;
  executionId: string;
  functionName: string;
  triggerName?: string;
  eventId?: string;
  startTime: string;
  endTime: string;
  status: 'success' | 'failure' | 'timeout';
  duration: number;
  result?: Record<string, unknown>;
  error?: string;
  logs: string[];
};

// Rate Limiting and Quota Types
export type RateLimit = {
  functionName: string;
  limit: number;
  period: 'minute' | 'hour' | 'day';
  current: number;
  resetTime: string;
};

export type QuotaUsage = {
  functions: {
    count: number;
    limit: number;
  };
  triggers: {
    count: number;
    limit: number;
  };
  executions: {
    count: number;
    limit: number;
  };
  period: 'month' | 'day';
  resetDate: string;
};

// Notification and Alert Types
export type FunctionAlert = {
  id: string;
  functionName: string;
  type: 'error' | 'timeout' | 'memory' | 'custom';
  condition: string;
  message: string;
  enabled: boolean;
  createdAt: string;
  lastTriggered?: string;
};

export type Notification = {
  id: string;
  type: 'alert' | 'info' | 'warning';
  title: string;
  message: string;
  read: boolean;
  timestamp: string;
  metadata?: Record<string, unknown>;
};

// Versioning and Deployment Types
export type FunctionVersion = {
  functionName: string;
  version: string;
  code: string;
  description?: string;
  deployedBy: string;
  deployedAt: string;
  checksum: string;
  isActive: boolean;
};

export type RollbackRequest = {
  functionName: string;
  targetVersion: string;
  reason: string;
};

// Test and Validation Types
export type FunctionTest = {
  functionName: string;
  testCases: TestCase[];
  results?: TestResult[];
  lastRun?: string;
};

export type TestCase = {
  id: string;
  name: string;
  input: Record<string, unknown>;
  expectedOutput?: Record<string, unknown>;
  expectedError?: string;
};

export type TestResult = {
  testCaseId: string;
  success: boolean;
  output?: Record<string, unknown>;
  error?: string;
  duration: number;
  timestamp: string;
};

// Add these specific types for the executor and event dispatch system
export type ExecutorStatus = {
  status: 'running' | 'stopped' | 'error';
  vmPoolSize: number;
  cachedFunctions: number;
  lastSync: string;
  syncInterval: number;
  circuitBreaker: {
    state: 'closed' | 'open' | 'half-open';
    failures: number;
    lastFailure?: string;
  };
};

export type EventDispatchStatus = {
  handlerStatus: 'active' | 'inactive';
  functionCache: {
    lastSync: string;
    count: number;
  };
  triggerCache: {
    lastSync: string;
    count: number;
  };
  processedEvents: number;
  activeExecutions: number;
};

// Add missing types for the task chain execution
export type TaskChainExecutionRequest = {
  chainId: string;
  input: Record<string, unknown>;
  dataType: DataType;
};

export type TaskChainExecutionResult = {
  chainId: string;
  result: unknown;
  resultType: DataType;
  executionHistory: ExecutionHistory;
  duration: number;
  success: boolean;
  error?: string;
};

// Add missing types for built-in function results
export type SendEventResult = {
  success: boolean;
  event_id?: string;
  error?: string;
};

export type CallTaskChainResult = {
  success: boolean;
  chain_id?: string;
  error?: string;
};

export type ExecuteTaskResult = {
  success: boolean;
  task_id?: string;
  response?: string;
  error?: string;
};

export type ExecuteTaskChainResult = {
  success: boolean;
  chain_id?: string;
  result?: unknown;
  history?: ExecutionHistory;
  error?: string;
};

// Add utility types for the UI
export type SortOrder = 'asc' | 'desc';
export type SortField = 'name' | 'createdAt' | 'updatedAt';

export type ListParams = PaginationParams & {
  sortBy?: SortField;
  sortOrder?: SortOrder;
  search?: string;
  filter?: Record<string, unknown>;
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
