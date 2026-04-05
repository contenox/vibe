import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../lib/api';
import { localHookKeys, remoteHookKeys } from '../lib/queryKeys';
import { LocalHook, RemoteHook } from '../lib/types';

export function useLocalHooks() {
  return useQuery<LocalHook[]>({
    queryKey: localHookKeys.list(),
    queryFn: () => api.getLocalHooks(),
  });
}

export function useRemoteHooks(params?: { limit?: number; cursor?: string }) {
  return useQuery<RemoteHook[]>({
    queryKey: remoteHookKeys.list(params || {}),
    queryFn: () => api.getRemoteHooks(params),
  });
}

// Get by ID
export function useRemoteHook(id: string) {
  return useQuery<RemoteHook>({
    queryKey: remoteHookKeys.detail(id),
    queryFn: () => api.getRemoteHook(id),
  });
}

// Get by name
export function useRemoteHookByName(name: string) {
  return useQuery<RemoteHook>({
    queryKey: remoteHookKeys.byName(name),
    queryFn: () => api.getRemoteHookByName(name),
  });
}

// Get schemas
export function useRemoteHookSchemas() {
  return useQuery<Record<string, unknown>>({
    queryKey: remoteHookKeys.schemas(),
    queryFn: () => api.getRemoteHookSchemas(),
  });
}

// Create
export function useCreateRemoteHook() {
  const queryClient = useQueryClient();
  return useMutation<RemoteHook, Error, Partial<RemoteHook>>({
    mutationFn: api.createRemoteHook,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: remoteHookKeys.all });
    },
  });
}

// Update
export function useUpdateRemoteHook() {
  const queryClient = useQueryClient();
  return useMutation<RemoteHook, Error, { id: string; data: Partial<RemoteHook> }>({
    mutationFn: ({ id, data }) => api.updateRemoteHook(id, data),
    onSuccess: (_, { id }) => {
      queryClient.invalidateQueries({ queryKey: remoteHookKeys.detail(id) });
      queryClient.invalidateQueries({ queryKey: remoteHookKeys.all });
    },
  });
}

// Delete
export function useDeleteRemoteHook() {
  const queryClient = useQueryClient();
  return useMutation<string, Error, string>({
    mutationFn: api.deleteRemoteHook,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: remoteHookKeys.all });
    },
  });
}
