import { useQuery } from '@tanstack/react-query';
import { api } from '../lib/api';
import { userKeys } from '../lib/queryKeys';
import { AuthenticatedUser } from '../lib/types';

export function useMe() {
  return useQuery<AuthenticatedUser>({
    queryKey: userKeys.current(),
    queryFn: api.getCurrentUser,
  });
}
