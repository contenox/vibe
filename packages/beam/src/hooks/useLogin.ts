import {
  UseMutationOptions,
  UseMutationResult,
  useMutation,
  useQueryClient,
} from '@tanstack/react-query';
import { useNavigate } from 'react-router-dom';
import { api } from '../lib/api';
import { userKeys } from '../lib/queryKeys';
import { AuthenticatedUser, User } from '../lib/types';

export function useLogin(
  options?: UseMutationOptions<AuthenticatedUser, Error, Partial<User>>,
): UseMutationResult<AuthenticatedUser, Error, Partial<User>, unknown> {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const defaultRedirect = '/';

  return useMutation<AuthenticatedUser, Error, Partial<User>, unknown>({
    mutationFn: api.login,
    onSuccess: (data, variables, context, onMutateResult) => {
      queryClient.setQueryData(userKeys.current(), data);
      queryClient.invalidateQueries({ queryKey: userKeys.current() });
      if (options?.onSuccess) {
        options.onSuccess(data, variables, context, onMutateResult);
      } else {
        navigate(defaultRedirect);
      }
    },
  });
}
