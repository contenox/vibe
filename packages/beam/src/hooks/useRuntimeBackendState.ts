import { useQuery } from '@tanstack/react-query';
import { api } from '../lib/api';

export function useRuntimeBackendState() {
  return useQuery({
    queryKey: ['runtime-backend-state'],
    queryFn: () => api.getRuntimeBackendState(),
    refetchInterval: 15_000,
  });
}
