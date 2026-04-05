import { apiFetch } from './fetch';
import {
  ActivePlanResponse,
  AuthenticatedUser,
  Backend,
  BackendRuntimeState,
  ChainDefinition,
  ChatMessage,
  ChatSession,
  CleanPlansResponse,
  CLIConfigUpdateResponse,
  CloudProviderType,
  Exec,
  ExecResp,
  FileResponse,
  FolderResponse,
  LocalHook,
  MCPServer,
  Model,
  NewPlanResponse,
  NextStepResponse,
  Plan,
  PlanMarkdownResponse,
  Pool,
  RemoteHook,
  ReplanResponse,
  SetupStatus,
  StateResponse,
  StatusResponse,
  TaskExecutionRequest,
  TaskExecutionResponse,
} from './types';

type HttpMethod = 'GET' | 'POST' | 'PUT' | 'DELETE';

interface ApiOptions {
  method?: HttpMethod;
  headers?: Record<string, string>;
  body?: string;
  credentials?: RequestCredentials;
}

const options = (method: HttpMethod, data?: unknown): ApiOptions => {
  const options: ApiOptions = {
    method,
    headers: { 'Content-Type': 'application/json' },
    credentials: 'same-origin',
  };

  if (data) {
    options.body = JSON.stringify(data);
  }

  return options;
};

interface FormDataApiOptions {
  method?: HttpMethod;
  headers?: Record<string, string>;
  body: FormData;
  credentials?: RequestCredentials;
}

type RawAuthenticatedUser = {
  id?: string;
  user_id?: string;
  subject?: string;
  username?: string;
  email?: string;
  friendlyName?: string;
  expiresAt?: string;
  expires_at?: string;
};

const formDataOptions = (method: HttpMethod, formData: FormData): FormDataApiOptions => {
  // NOTE: We DO NOT set 'Content-Type': 'multipart/form-data'.
  // The browser sets it automatically along with the correct boundary when using FormData.
  return {
    method,
    body: formData,
    credentials: 'same-origin',
    // headers: {} // Intentionally omitted for Content-Type
  };
};

const normalizeAuthenticatedUser = (raw: RawAuthenticatedUser): AuthenticatedUser => {
  const username = raw.username ?? raw.friendlyName ?? raw.email ?? 'admin';
  const id = raw.id ?? raw.user_id ?? raw.subject ?? username;
  return {
    id,
    subject: raw.subject ?? raw.user_id ?? id,
    email: raw.email ?? username,
    friendlyName: raw.friendlyName ?? username,
    username,
    expiresAt: raw.expiresAt ?? raw.expires_at,
  };
};

export const api = {
  // Remote Hooks
  getRemoteHooks: (params?: { limit?: number; cursor?: string }) => {
    const search = new URLSearchParams();
    if (params?.limit !== undefined) search.set('limit', params.limit.toString());
    if (params?.cursor) search.set('cursor', params.cursor);
    const qs = search.toString() ? `?${search.toString()}` : '';
    return apiFetch<RemoteHook[]>(`/api/hooks/remote${qs}`);
  },

  getRemoteHook: (id: string) => apiFetch<RemoteHook>(`/api/hooks/remote/${id}`),

  getRemoteHookByName: (name: string) => apiFetch<RemoteHook>(`/api/hooks/remote/by-name/${name}`),

  createRemoteHook: (data: Partial<RemoteHook>) =>
    apiFetch<RemoteHook>('/api/hooks/remote', options('POST', data)),

  updateRemoteHook: (id: string, data: Partial<RemoteHook>) =>
    apiFetch<RemoteHook>(`/api/hooks/remote/${id}`, options('PUT', data)),

  deleteRemoteHook: (id: string) => apiFetch<string>(`/api/hooks/remote/${id}`, options('DELETE')),
  getLocalHooks: () => apiFetch<LocalHook[]>('/api/hooks/local'),

  getRemoteHookSchemas: () => apiFetch<Record<string, unknown>>('/api/hooks/schemas'),

  // MCP servers (persisted configs; same DB as `contenox mcp`)
  getMcpServers: (params?: { limit?: number; cursor?: string }) => {
    const search = new URLSearchParams();
    if (params?.limit !== undefined) search.set('limit', params.limit.toString());
    if (params?.cursor) search.set('cursor', params.cursor);
    const qs = search.toString() ? `?${search.toString()}` : '';
    return apiFetch<MCPServer[]>(`/api/mcp-servers${qs}`);
  },
  getMcpServer: (id: string) => apiFetch<MCPServer>(`/api/mcp-servers/${id}`),
  getMcpServerByName: (name: string) =>
    apiFetch<MCPServer>(`/api/mcp-servers/by-name/${encodeURIComponent(name)}`),
  createMcpServer: (data: Partial<MCPServer>) =>
    apiFetch<MCPServer>('/api/mcp-servers', options('POST', data)),
  updateMcpServer: (id: string, data: Partial<MCPServer>) =>
    apiFetch<MCPServer>(`/api/mcp-servers/${id}`, options('PUT', data)),
  deleteMcpServer: (id: string) => apiFetch<string>(`/api/mcp-servers/${id}`, options('DELETE')),
  /** Starts OAuth 2.1 PKCE for an oauth-type MCP server; open authorizationUrl in the browser. */
  startMcpOAuth: (id: string, body: { redirectBase: string }) =>
    apiFetch<{ authorizationUrl: string }>(
      `/api/mcp-servers/${id}/oauth/start`,
      options('POST', body),
    ),
  // Backends
  getBackends: () => apiFetch<Backend[]>('/api/backends'),
  getBackend: (id: string) => apiFetch<Backend>(`/api/backends/${id}`),
  createBackend: (data: Partial<Backend>) =>
    apiFetch<Backend>('/api/backends', options('POST', data)),
  updateBackend: (id: string, data: Partial<Backend>) =>
    apiFetch<Backend>(`/api/backends/${id}`, options('PUT', data)),
  deleteBackend: (id: string) => apiFetch<void>(`/api/backends/${id}`, options('DELETE')),

  getSetupStatus: async (): Promise<SetupStatus> => {
    const raw = await apiFetch<SetupStatus>('/api/setup-status');
    return {
      ...raw,
      defaultModel: raw.defaultModel ?? '',
      defaultProvider: raw.defaultProvider ?? '',
      defaultChain: raw.defaultChain ?? '',
      backendCount: raw.backendCount ?? 0,
      reachableBackendCount: raw.reachableBackendCount ?? 0,
      issues: Array.isArray(raw.issues) ? raw.issues : [],
      backendChecks: Array.isArray(raw.backendChecks) ? raw.backendChecks : [],
    };
  },
  putCLIConfig: (body: {
    'default-model'?: string;
    'default-provider'?: string;
    'default-chain'?: string;
  }) => apiFetch<CLIConfigUpdateResponse>('/api/cli-config', options('PUT', body)),

  // Chats
  createChat: ({ model }: Partial<ChatSession>) =>
    apiFetch<Partial<ChatSession>>('/api/chats', options('POST', { model })),

  sendMessage: (
    id: string,
    message: string,
    chainId: string,
    opts?: { model?: string; provider?: string; signal?: AbortSignal; requestId?: string },
  ) => {
    const params = new URLSearchParams();
    if (chainId) params.append('chainId', chainId);
    if (opts?.model) params.append('model', opts.model);
    if (opts?.provider) params.append('provider', opts.provider);

    const requestOptions = options('POST', { message });
    if (opts?.requestId) {
      requestOptions.headers = {
        ...requestOptions.headers,
        'X-Request-ID': opts.requestId,
      };
    }

    return apiFetch<StateResponse>(`/api/chats/${id}/chat?${params.toString()}`, {
      ...requestOptions,
      signal: opts?.signal,
    });
  },

  getChatHistory: (id: string) => apiFetch<ChatMessage[]>(`/api/chats/${id}`),
  getChats: () => apiFetch<ChatSession[]>('/api/chats'),

  /** Runtime sync snapshot per backend (OSS backend refresh loop; not a managed download queue). */
  getRuntimeBackendState: () => apiFetch<BackendRuntimeState[]>('/api/state'),
  taskEvents(requestId: string): EventSource {
    // Must be root-absolute: on routes like /chat/:id, a relative "api/..." resolves to
    // /chat/api/... and hits the SPA shell instead of the API mux.
    return new EventSource(`/api/task-events?requestId=${encodeURIComponent(requestId)}`);
  },

  // Pools
  getPools: () => apiFetch<Pool[]>('/api/groups'),
  getPool: (id: string) => apiFetch<Pool>(`/api/groups/${id}`),
  createPool: (data: Partial<Pool>) => apiFetch<Pool>('/api/groups', options('POST', data)),
  updatePool: (id: string, data: Partial<Pool>) =>
    apiFetch<Pool>(`/api/groups/${id}`, options('PUT', data)),
  deletePool: (id: string) => apiFetch<void>(`/api/groups/${id}`, options('DELETE')),
  getPoolByName: (name: string) => apiFetch<Pool>(`/api/group-by-name/${name}`),
  listPoolsByPurpose: (purpose: string) => apiFetch<Pool[]>(`/api/group-by-purpose/${purpose}`),

  // Backend associations
  assignBackendToPool: (poolID: string, backendID: string) =>
    apiFetch<void>(`/api/backend-affinity/${poolID}/backends/${backendID}`, options('POST')),
  removeBackendFromPool: (poolID: string, backendID: string) =>
    apiFetch<void>(`/api/backend-affinity/${poolID}/backends/${backendID}`, options('DELETE')),
  listBackendsForPool: (poolID: string) =>
    apiFetch<Backend[]>(`/api/backend-affinity/${poolID}/backends`),
  listPoolsForBackend: (backendID: string) =>
    apiFetch<Pool[]>(`/api/backend-affinity/${backendID}/groups`),

  // Model associations
  assignModelToPool: (poolID: string, modelID: string) =>
    apiFetch<void>(`/api/model-affinity/${poolID}/models/${modelID}`, options('POST')),
  removeModelFromPool: (poolID: string, modelID: string) =>
    apiFetch<void>(`/api/model-affinity/${poolID}/models/${modelID}`, options('DELETE')),
  listModelsForPool: (poolID: string) => apiFetch<Model[]>(`/api/model-affinity/${poolID}/models`),
  listPoolsForModel: (modelID: string) => apiFetch<Pool[]>(`/api/model-affinity/${modelID}/groups`),

  // Add to the api object:
  configureProvider: (provider: CloudProviderType, data: { apiKey: string; upsert: boolean }) =>
    apiFetch<StatusResponse>(`/api/providers/${provider}/configure`, options('POST', data)),

  getProviderStatus: (provider: CloudProviderType) =>
    apiFetch<StatusResponse>(`/api/providers/${provider}/status`),

  // Auth endpoints
  login: async (data: { email?: string; password?: string }): Promise<AuthenticatedUser> =>
    normalizeAuthenticatedUser(
      await apiFetch<RawAuthenticatedUser>('/api/ui/login', options('POST', data)),
    ),
  logout: () => apiFetch<void>('/api/ui/logout', options('POST')),
  getCurrentUser: async (): Promise<AuthenticatedUser> =>
    normalizeAuthenticatedUser(await apiFetch<RawAuthenticatedUser>('/api/ui/me')),

  // File management
  getFileMetadata: (id: string) => apiFetch<FileResponse>(`/api/files/${id}`),

  createFile: (formData: FormData) =>
    apiFetch<FileResponse>('/api/files', formDataOptions('POST', formData)),

  updateFile: (id: string, formData: FormData) =>
    apiFetch<FileResponse>(`/api/files/${id}`, formDataOptions('PUT', formData)),

  deleteFile: (id: string) => apiFetch<void>(`/api/files/${id}`, options('DELETE')),

  getDownloadFileUrl: (id: string) => `/api/files/${id}/download`,

  listFiles: (path?: string) => {
    const query = path ? `?path=${encodeURIComponent(path)}` : '';
    return apiFetch<FileResponse[]>(`/api/files${query}`);
  },
  // Folder management
  createFolder: (data: { path: string }) =>
    apiFetch<FolderResponse>('/api/folders', options('POST', data)),

  renameFolder: (id: string, data: { path: string }) =>
    apiFetch<FolderResponse>(`/api/folders/${id}/path`, options('PUT', data)),

  renameFile: (id: string, data: { path: string }) =>
    apiFetch<FileResponse>(`/api/files/${id}/path`, options('PUT', data)),
  execPrompt: (data: Exec) => apiFetch<ExecResp>(`/api/execute`, options('POST', data)),
  executeTaskChain: (
    data: TaskExecutionRequest,
    opts?: { signal?: AbortSignal; requestId?: string },
  ) => {
    const requestOptions = options('POST', data);
    if (opts?.requestId) {
      requestOptions.headers = {
        ...requestOptions.headers,
        'X-Request-ID': opts.requestId,
      };
    }
    return apiFetch<TaskExecutionResponse>(`/api/tasks`, {
      ...requestOptions,
      signal: opts?.signal,
    });
  },

  /** Task chains live in the VFS; list files with GET /api/files. Path is relative (e.g. default-chain.json). */
  getChain: (path: string) =>
    apiFetch<ChainDefinition>(`/api/taskchains?path=${encodeURIComponent(path)}`),
  createChain: (path: string, data: Partial<ChainDefinition>) =>
    apiFetch<ChainDefinition>(
      `/api/taskchains?path=${encodeURIComponent(path)}`,
      options('POST', data),
    ),
  updateChain: (path: string, data: Partial<ChainDefinition>) =>
    apiFetch<ChainDefinition>(
      `/api/taskchains?path=${encodeURIComponent(path)}`,
      options('PUT', data),
    ),
  deleteChain: (path: string) =>
    apiFetch<void>(`/api/taskchains?path=${encodeURIComponent(path)}`, options('DELETE')),

  /** Autonomous plans (same DB as `contenox plan`). */
  listPlans: () => apiFetch<Plan[]>('/api/plans'),
  getActivePlan: () => apiFetch<ActivePlanResponse>('/api/plans/active'),
  createPlan: (body: { goal: string; planner_chain_id: string }) =>
    apiFetch<NewPlanResponse>('/api/plans', options('POST', body)),
  planNext: (body: { executor_chain_id: string; with_shell?: boolean; with_auto?: boolean }) =>
    apiFetch<NextStepResponse>('/api/plans/active/next', options('POST', body)),
  planReplan: (body: { planner_chain_id: string }) =>
    apiFetch<ReplanResponse>('/api/plans/active/replan', options('POST', body)),
  retryPlanStep: (ordinal: number) =>
    apiFetch<PlanMarkdownResponse>(`/api/plans/active/steps/${ordinal}/retry`, options('POST', {})),
  skipPlanStep: (ordinal: number) =>
    apiFetch<PlanMarkdownResponse>(`/api/plans/active/steps/${ordinal}/skip`, options('POST', {})),
  activatePlan: (name: string) =>
    apiFetch<string>(`/api/plans/${encodeURIComponent(name)}/activate`, options('PUT', {})),
  deletePlan: (name: string) =>
    apiFetch<string>(`/api/plans/${encodeURIComponent(name)}`, options('DELETE')),
  cleanPlans: () => apiFetch<CleanPlansResponse>('/api/plans/clean', options('POST', {})),
};
