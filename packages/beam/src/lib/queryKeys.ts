export const poolKeys = {
  all: ['pools'] as const,
  detail: (id: string) => [...poolKeys.all, id] as const,
  backends: (poolID: string) => [...poolKeys.all, poolID, 'backends'] as const,
  models: (poolID: string) => [...poolKeys.all, poolID, 'models'] as const,
  byPurpose: (purpose: string) => [...poolKeys.all, 'purpose', purpose] as const,
  byName: (name: string) => [...poolKeys.all, 'name', name] as const,
};

export const backendKeys = {
  all: ['backends'] as const,
  detail: (id: string) => [...backendKeys.all, id] as const,
  pools: (backendID: string) => [...backendKeys.all, backendID, 'pools'] as const,
};

export const githubKeys = {
  all: ['github'] as const,
  repos: () => [...githubKeys.all, 'repos'] as const,
  repo: (id: string) => [...githubKeys.all, 'repo', id] as const,
  prs: (repoID: string) => [...githubKeys.all, 'prs', repoID] as const,
};

export const providerKeys = {
  status: (provider: string) => ['providers', provider, 'status'] as const,
};

export const stateKeys = {
  all: ['state'] as const,
  pending: () => [...stateKeys.all, 'pending'],
  inProgress: () => [...stateKeys.all, 'inprogress'],
};

export const folderKeys = {
  all: ['folders'] as const,
  lists: () => [...folderKeys.all, 'list'] as const,
  details: () => [...folderKeys.all, 'detail'] as const,
  detail: (id: string) => [...fileKeys.details(), id] as const,
};

export const fileKeys = {
  all: ['files'] as const,
  lists: () => [...fileKeys.all, 'list'] as const,
  details: () => [...fileKeys.all, 'detail'] as const,
  detail: (id: string) => [...fileKeys.details(), id] as const,
  paths: () => [...fileKeys.all, 'paths'] as const,
};

export const jobKeys = {
  all: ['jobs'] as const,
  pending: () => [...jobKeys.all, 'pending'],
  inprogress: () => [...jobKeys.all, 'inprogress'],
};

export const accessKeys = {
  all: ['accessEntries'] as const,
  list: (expand: boolean, identity?: string) => [...accessKeys.all, { expand, identity }] as const,
};

export const permissionKeys = {
  all: ['perms'] as const,
};

export const chatKeys = {
  all: ['chats'] as const,
  history: (chatId: string) => [...chatKeys.all, 'history', chatId] as const,
};

export const setupKeys = {
  all: ['setup'] as const,
  status: () => [...setupKeys.all, 'status'] as const,
};

export const userKeys = {
  all: ['users'] as const,
  current: () => [...userKeys.all, 'current'],
  list: (from?: string) => [...userKeys.all, 'list', { from }] as const,
};

export const systemKeys = {
  all: ['system'] as const,
  resources: () => [...systemKeys.all, 'resources'],
};

export const searchKeys = {
  all: ['search'] as const,
  query: (params: { query: string; topk?: number; radius?: number; epsilon?: number }) =>
    [...searchKeys.all, params] as const,
};

export const execKeys = {
  all: ['exec'] as const,
};

export const typeKeys = {
  all: ['types'] as const,
};

export const chainKeys = {
  all: ['chains'] as const,
  /** VFS-relative path (e.g. my-chain.json) */
  byPath: (vfsPath: string) => [...chainKeys.all, 'path', vfsPath] as const,
  triggers: (vfsPath: string) => [...chainKeys.byPath(vfsPath), 'triggers'] as const,
  tasks: (vfsPath: string) => [...chainKeys.byPath(vfsPath), 'tasks'] as const,
  triggerDetail: (vfsPath: string, triggerId: string) =>
    [...chainKeys.triggers(vfsPath), triggerId] as const,
};

export const activityKeys = {
  all: ['activity'] as const,
  logs: (limit?: number) => ['activity', 'logs', { limit }] as const,
  requests: (limit?: number) => ['activity', 'requests', { limit }] as const,
  requestById: (requestID: string) => ['activity', 'requests', 'detail', requestID] as const,
  operations: () => ['activity', 'operations'] as const,
  operationsByType: (operation: string, subject: string) =>
    ['activity', 'operations', 'detail', operation, subject] as const,
  state: (requestID: string) => ['activity', 'state', requestID] as const,
  statefulRequests: () => ['activity', 'statefulRequests'] as const,
  alerts: (limit?: number) => ['activity', 'alerts', { limit }] as const,
};

export const telegramKeys = {
  all: ['telegramFrontends'] as const,
  detail: (id: string) => [...telegramKeys.all, id] as const,
  list: () => [...telegramKeys.all, 'list'] as const,
  byUser: (userId: string) => [...telegramKeys.all, 'user', userId] as const,
};

export const botKeys = {
  all: ['bots'] as const,
  list: () => [...botKeys.all, 'list'] as const,
  detail: (id: string) => [...botKeys.all, id] as const,
};
export const eventKeys = {
  all: ['events'] as const,
  lists: () => [...eventKeys.all, 'list'] as const,
  list: (params: {
    event_type?: string;
    from?: string;
    to?: string;
    limit?: number;
    aggregate_type?: string;
    aggregate_id?: string;
    event_source?: string;
  }) => [...eventKeys.lists(), params] as const,
  detail: (id: string) => [...eventKeys.all, 'detail', id] as const,
  types: (params: { from?: string; to?: string; limit?: number }) =>
    [...eventKeys.all, 'types', params] as const,
  byAggregate: (params: {
    event_type: string;
    aggregate_type: string;
    aggregate_id: string;
    from?: string;
    to?: string;
    limit?: number;
  }) => [...eventKeys.all, 'aggregate', params] as const,
  byType: (params: { event_type: string; from?: string; to?: string; limit?: number }) =>
    [...eventKeys.all, 'type', params] as const,
  bySource: (params: {
    event_type: string;
    event_source: string;
    from?: string;
    to?: string;
    limit?: number;
  }) => [...eventKeys.all, 'source', params] as const,
};

export const rawEventKeys = {
  all: ['rawEvents'] as const,
  lists: () => [...rawEventKeys.all, 'list'] as const,
  list: (params: { from?: string; to?: string; limit?: number }) =>
    [...rawEventKeys.lists(), params] as const,
  detail: (nid: number, from: string, to: string) =>
    [...rawEventKeys.all, 'detail', nid, from, to] as const,
};

export const mappingKeys = {
  all: ['mappings'] as const,
  list: () => [...mappingKeys.all, 'list'] as const,
  detail: (path: string) => [...mappingKeys.all, 'detail', path] as const,
};

export const functionKeys = {
  all: ['functions'] as const,
  lists: () => [...functionKeys.all, 'list'] as const,
  list: (params?: { limit?: number; cursor?: string }) =>
    [...functionKeys.lists(), params] as const,
  detail: (name: string) => [...functionKeys.all, 'detail', name] as const,
};

export const localHookKeys = {
  all: ['localHooks'] as const,
  list: () => [...localHookKeys.all, 'list'] as const,
};

export const eventTriggerKeys = {
  all: ['eventTriggers'] as const,
  lists: () => [...eventTriggerKeys.all, 'list'] as const,
  list: (params?: { limit?: number; cursor?: string }) =>
    [...eventTriggerKeys.lists(), params] as const,
  detail: (name: string) => [...eventTriggerKeys.all, 'detail', name] as const,
  byEventType: (eventType: string) => [...eventTriggerKeys.all, 'byEventType', eventType] as const,
  byFunction: (functionName: string) =>
    [...eventTriggerKeys.all, 'byFunction', functionName] as const,
};

export const executorKeys = {
  all: ['executor'] as const,
  sync: () => [...executorKeys.all, 'sync'] as const,
};

export const remoteHookKeys = {
  all: ['remoteHooks'] as const,
  list: (params: { limit?: number; cursor?: string }) =>
    [...remoteHookKeys.all, 'list', params] as const,
  detail: (id: string) => [...remoteHookKeys.all, id] as const,
  byName: (name: string) => [...remoteHookKeys.all, 'name', name] as const,
  schemas: () => [...remoteHookKeys.all, 'schemas'] as const,
};

export const mcpServerKeys = {
  all: ['mcpServers'] as const,
  list: (params: { limit?: number; cursor?: string }) =>
    [...mcpServerKeys.all, 'list', params] as const,
  detail: (id: string) => [...mcpServerKeys.all, id] as const,
  byName: (name: string) => [...mcpServerKeys.all, 'name', name] as const,
};

export const terminalKeys = {
  all: ['terminal'] as const,
  sessions: () => [...terminalKeys.all, 'sessions'] as const,
  session: (id: string) => [...terminalKeys.all, 'session', id] as const,
};

export const planKeys = {
  all: ['plans'] as const,
  list: () => [...planKeys.all, 'list'] as const,
  active: () => [...planKeys.all, 'active'] as const,
  compilePreviewPrefix: () => [...planKeys.all, 'compile-preview'] as const,
  compilePreview: (planId: string, executorChainId: string, stepsDigest: string) =>
    [...planKeys.compilePreviewPrefix(), planId, executorChainId, stepsDigest] as const,
};
