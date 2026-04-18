import { useMutation, useQuery, useQueryClient, UseQueryResult } from '@tanstack/react-query';
import { api } from '../lib/api';
import { hitlPolicyKeys, setupKeys } from '../lib/queryKeys';
import type { HITLPolicy } from '../lib/types';

export function useListPolicies(): UseQueryResult<string[], Error> {
  return useQuery<string[], Error>({
    queryKey: hitlPolicyKeys.list(),
    queryFn: () => api.listPolicies(),
  });
}

export function usePolicy(name: string) {
  return useQuery<HITLPolicy>({
    queryKey: hitlPolicyKeys.byName(name),
    queryFn: () => api.getPolicy(name),
    enabled: !!name,
  });
}

export function useCreatePolicy() {
  const queryClient = useQueryClient();
  return useMutation<HITLPolicy, Error, { name: string; policy: HITLPolicy }>({
    mutationFn: ({ name, policy }) => api.createPolicy(name, policy),
    onSuccess: (_data, { name }) => {
      queryClient.invalidateQueries({ queryKey: hitlPolicyKeys.list() });
      queryClient.invalidateQueries({ queryKey: hitlPolicyKeys.byName(name) });
    },
  });
}

export function useUpdatePolicy(name: string) {
  const queryClient = useQueryClient();
  return useMutation<HITLPolicy, Error, HITLPolicy>({
    mutationFn: policy => api.updatePolicy(name, policy),
    onSuccess: updated => {
      queryClient.invalidateQueries({ queryKey: hitlPolicyKeys.list() });
      queryClient.setQueryData(hitlPolicyKeys.byName(name), updated);
    },
  });
}

export function useDeletePolicy() {
  const queryClient = useQueryClient();
  return useMutation<string, Error, string>({
    mutationFn: name => api.deletePolicy(name),
    onSuccess: (_, name) => {
      queryClient.invalidateQueries({ queryKey: hitlPolicyKeys.list() });
      queryClient.removeQueries({ queryKey: hitlPolicyKeys.byName(name) });
    },
  });
}

export function useSetActivePolicy() {
  const queryClient = useQueryClient();
  return useMutation<void, Error, string>({
    mutationFn: async name => {
      await api.putCLIConfig({ 'hitl-policy-name': name });
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: setupKeys.status() });
    },
  });
}
