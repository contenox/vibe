import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../lib/api';
import { workspaceKeys } from '../lib/queryKeys';
import type { Workspace } from '../lib/types';

export function useWorkspaces() {
  return useQuery<Workspace[]>({
    queryKey: workspaceKeys.list(),
    queryFn: api.listWorkspaces,
  });
}

export function useWorkspace(id: string, options?: { enabled?: boolean }) {
  return useQuery<Workspace>({
    queryKey: workspaceKeys.detail(id),
    queryFn: () => api.getWorkspace(id),
    enabled: options?.enabled ?? !!id,
  });
}

export function useCreateWorkspace() {
  const qc = useQueryClient();
  return useMutation<Workspace, Error, { name: string; path: string; shell?: string }>({
    mutationFn: body => api.createWorkspace(body),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: workspaceKeys.list() });
    },
  });
}

export function useUpdateWorkspace() {
  const qc = useQueryClient();
  return useMutation<
    Workspace,
    Error,
    { id: string; body: { name: string; path: string; shell?: string } }
  >({
    mutationFn: ({ id, body }) => api.updateWorkspace(id, body),
    onSuccess: (_, { id }) => {
      qc.invalidateQueries({ queryKey: workspaceKeys.list() });
      qc.invalidateQueries({ queryKey: workspaceKeys.detail(id) });
    },
  });
}

export function useDeleteWorkspace() {
  const qc = useQueryClient();
  return useMutation<void, Error, string>({
    mutationFn: api.deleteWorkspace,
    onSuccess: (_, deletedId) => {
      qc.invalidateQueries({ queryKey: workspaceKeys.list() });
      qc.removeQueries({ queryKey: workspaceKeys.detail(deletedId) });
    },
  });
}
