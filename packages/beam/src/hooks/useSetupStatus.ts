import { useQuery } from '@tanstack/react-query';
import { api } from '../lib/api';
import { setupKeys } from '../lib/queryKeys';

export function useSetupStatus(enabled: boolean) {
  return useQuery({
    queryKey: setupKeys.status(),
    queryFn: api.getSetupStatus,
    enabled,
    staleTime: 30_000,
  });
}
