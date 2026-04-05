import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../lib/api';
import { providerKeys, setupKeys } from '../lib/queryKeys';
import type { CloudProviderType } from '../lib/types';

export function useProviderStatus(provider: CloudProviderType) {
  return useQuery({
    queryKey: providerKeys.status(provider),
    queryFn: () => api.getProviderStatus(provider),
    refetchInterval: 5000, // Refresh every 5 seconds
  });
}

export function useConfigureProvider(provider: CloudProviderType) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (data: { apiKey: string; upsert: boolean }) =>
      api.configureProvider(provider, data),
    onSuccess: () => {
      queryClient.invalidateQueries({
        queryKey: providerKeys.status(provider),
      });
      queryClient.invalidateQueries({ queryKey: setupKeys.status() });
    },
  });
}
