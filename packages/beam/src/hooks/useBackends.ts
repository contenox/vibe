import {
  useMutation,
  UseMutationResult,
  useQueryClient,
  useSuspenseQuery,
} from '@tanstack/react-query';
import { api } from '../lib/api';
import { backendKeys, setupKeys } from '../lib/queryKeys';
import { Backend } from '../lib/types';

export function useBackends() {
  return useSuspenseQuery<Backend[]>({
    queryKey: backendKeys.all,
    queryFn: api.getBackends,
    refetchInterval: 3000,
  });
}

export function useCreateBackend(): UseMutationResult<Backend, Error, Partial<Backend>, unknown> {
  const queryClient = useQueryClient();
  return useMutation<Backend, Error, Partial<Backend>>({
    mutationFn: api.createBackend,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: backendKeys.all });
      queryClient.invalidateQueries({ queryKey: setupKeys.status() });
    },
  });
}

export function useDeleteBackend(): UseMutationResult<void, Error, string, unknown> {
  const queryClient = useQueryClient();
  return useMutation<void, Error, string>({
    mutationFn: api.deleteBackend,
    onSettled: () => {
      queryClient.invalidateQueries({ queryKey: backendKeys.all });
      queryClient.invalidateQueries({ queryKey: setupKeys.status() });
    },
  });
}

export function useUpdateBackend() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, data }: { id: string; data: Partial<Backend> }) =>
      api.updateBackend(id, data),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: backendKeys.all });
      queryClient.invalidateQueries({ queryKey: setupKeys.status() });
    },
  });
}
