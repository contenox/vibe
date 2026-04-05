import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '../lib/api';
import { setupKeys } from '../lib/queryKeys';
import { CLIConfigUpdateResponse } from '../lib/types';

export function usePutCLIConfig() {
  const queryClient = useQueryClient();
  return useMutation<
    CLIConfigUpdateResponse,
    Error,
    { 'default-model'?: string; 'default-provider'?: string; 'default-chain'?: string }
  >({
    mutationFn: body => api.putCLIConfig(body),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: setupKeys.status() });
    },
  });
}
