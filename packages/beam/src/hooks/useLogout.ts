import {
  useMutation,
  UseMutationOptions,
  UseMutationResult,
  useQueryClient,
} from '@tanstack/react-query';
import { useNavigate } from 'react-router-dom';
import { api } from '../lib/api';
import { userKeys } from '../lib/queryKeys';

export function useLogout(
  options?: UseMutationOptions<void, Error, void>,
): UseMutationResult<void, Error, void, unknown> {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const defaultRedirect = '/bye';

  return useMutation<void, Error, void, unknown>({
    mutationFn: api.logout,

    onSuccess: (data, variables, context, onMutateResult) => {
      queryClient.resetQueries({ queryKey: userKeys.current() });

      if (options?.onSuccess) {
        options.onSuccess(data, variables, context, onMutateResult);
      } else {
        navigate(defaultRedirect);
      }
    },
  });
}
