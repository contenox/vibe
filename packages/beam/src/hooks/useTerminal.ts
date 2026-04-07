import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../lib/api';
import { terminalKeys } from '../lib/queryKeys';
import type { TerminalSession, TerminalSessionCreate } from '../lib/types';

export function useTerminalSessions() {
  return useQuery<TerminalSession[]>({
    queryKey: terminalKeys.sessions(),
    queryFn: api.listTerminalSessions,
  });
}

export function useTerminalSession(id: string, options?: { enabled?: boolean }) {
  return useQuery<TerminalSession>({
    queryKey: terminalKeys.session(id),
    queryFn: () => api.getTerminalSession(id),
    enabled: options?.enabled ?? !!id,
  });
}

export function useCreateTerminalSession() {
  const queryClient = useQueryClient();
  return useMutation<TerminalSessionCreate, Error, { cwd: string; cols?: number; rows?: number }>({
    mutationFn: body => api.createTerminalSession(body),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: terminalKeys.sessions() });
    },
  });
}

export function useDeleteTerminalSession() {
  const queryClient = useQueryClient();
  return useMutation<void, Error, string>({
    mutationFn: api.deleteTerminalSession,
    onSuccess: (_, deletedId) => {
      queryClient.invalidateQueries({ queryKey: terminalKeys.sessions() });
      queryClient.removeQueries({ queryKey: terminalKeys.session(deletedId) });
    },
  });
}
