import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../lib/api';
import { localHookKeys, mcpServerKeys } from '../lib/queryKeys';
import { MCPServer } from '../lib/types';

const defaultListParams = { limit: 100 as number | undefined, cursor: undefined as string | undefined };

export function useMcpServers(params?: { limit?: number; cursor?: string }) {
  const p = { ...defaultListParams, ...params };
  return useQuery<MCPServer[]>({
    queryKey: mcpServerKeys.list(p),
    queryFn: () => api.getMcpServers(p),
  });
}

export function useMcpServer(id: string, options?: { enabled?: boolean }) {
  return useQuery<MCPServer>({
    queryKey: mcpServerKeys.detail(id),
    queryFn: () => api.getMcpServer(id),
    enabled: options?.enabled ?? !!id,
  });
}

export function useCreateMcpServer() {
  const queryClient = useQueryClient();
  return useMutation<MCPServer, Error, Partial<MCPServer>>({
    mutationFn: api.createMcpServer,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: mcpServerKeys.all });
      queryClient.invalidateQueries({ queryKey: localHookKeys.list() });
    },
  });
}

export function useUpdateMcpServer() {
  const queryClient = useQueryClient();
  return useMutation<MCPServer, Error, { id: string; data: Partial<MCPServer> }>({
    mutationFn: ({ id, data }) => api.updateMcpServer(id, data),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: mcpServerKeys.all });
      queryClient.invalidateQueries({ queryKey: localHookKeys.list() });
    },
  });
}

export function useDeleteMcpServer() {
  const queryClient = useQueryClient();
  return useMutation<string, Error, string>({
    mutationFn: api.deleteMcpServer,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: mcpServerKeys.all });
      queryClient.invalidateQueries({ queryKey: localHookKeys.list() });
    },
  });
}

export function useStartMcpOAuth() {
  const queryClient = useQueryClient();
  return useMutation<{ authorizationUrl: string }, Error, { id: string; redirectBase: string }>({
    mutationFn: ({ id, redirectBase }) => api.startMcpOAuth(id, { redirectBase }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: mcpServerKeys.all });
      queryClient.invalidateQueries({ queryKey: localHookKeys.list() });
    },
  });
}
