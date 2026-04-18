import { useMutation, useQueryClient, useSuspenseQuery } from '@tanstack/react-query';
import { api } from '../lib/api';
import { modelRegistryKeys } from '../lib/queryKeys';
import { ModelRegistryEntry } from '../lib/types';

export function useModelRegistry() {
  return useSuspenseQuery({
    queryKey: modelRegistryKeys.all,
    queryFn: api.listModelRegistry,
  });
}

export function useCreateModelRegistryEntry() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (data: Omit<ModelRegistryEntry, 'id' | 'createdAt' | 'updatedAt'>) =>
      api.createModelRegistryEntry(data),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: modelRegistryKeys.all });
    },
  });
}

export function useDeleteModelRegistryEntry() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.deleteModelRegistryEntry(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: modelRegistryKeys.all });
    },
  });
}

export function useDownloadModel() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.downloadModel(name),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: modelRegistryKeys.all });
    },
  });
}
