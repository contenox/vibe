import {
  useMutation,
  UseMutationResult,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query';
import { api } from '../lib/api';
import { chainKeys, fileKeys } from '../lib/queryKeys';
import { ChainDefinition, ChainTask } from '../lib/types';

/** Load a single chain JSON from the VFS path (required query param on GET /api/taskchains). */
export function useChain(vfsPath: string) {
  return useQuery<ChainDefinition>({
    queryKey: chainKeys.byPath(vfsPath),
    queryFn: () => api.getChain(vfsPath),
    enabled: !!vfsPath,
  });
}

export type CreateChainInput = { vfsPath: string; chain: ChainDefinition };

export function useCreateChain(): UseMutationResult<
  ChainDefinition,
  Error,
  CreateChainInput,
  unknown
> {
  const queryClient = useQueryClient();
  return useMutation<ChainDefinition, Error, CreateChainInput>({
    mutationFn: ({ vfsPath, chain }) => api.createChain(vfsPath, chain),
    onSuccess: (_data, { vfsPath }) => {
      queryClient.invalidateQueries({ queryKey: fileKeys.lists() });
      queryClient.invalidateQueries({ queryKey: chainKeys.byPath(vfsPath) });
    },
  });
}

export function useUpdateChain(vfsPath: string) {
  const queryClient = useQueryClient();
  return useMutation<ChainDefinition, Error, Partial<ChainDefinition>, unknown>({
    mutationFn: data => api.updateChain(vfsPath, data),
    onSuccess: updatedChain => {
      queryClient.invalidateQueries({ queryKey: fileKeys.lists() });
      queryClient.setQueryData(chainKeys.byPath(vfsPath), updatedChain);
    },
  });
}

export function useDeleteChain(): UseMutationResult<void, Error, string, unknown> {
  const queryClient = useQueryClient();
  return useMutation<void, Error, string>({
    mutationFn: vfsPath => api.deleteChain(vfsPath),
    onSuccess: (_, vfsPath) => {
      queryClient.invalidateQueries({ queryKey: fileKeys.lists() });
      queryClient.removeQueries({ queryKey: chainKeys.byPath(vfsPath) });
    },
  });
}

export function useUpdateChainTask(vfsPath: string) {
  const queryClient = useQueryClient();
  return useMutation<ChainDefinition, Error, { taskId: string; data: Partial<ChainTask> }, unknown>(
    {
      mutationFn: async ({ taskId, data }) => {
        const chain = await queryClient.fetchQuery({
          queryKey: chainKeys.byPath(vfsPath),
          queryFn: () => api.getChain(vfsPath),
        });

        const updatedTasks = chain.tasks.map(task =>
          task.id === taskId ? { ...task, ...data } : task,
        );

        return api.updateChain(vfsPath, { tasks: updatedTasks });
      },
      onSuccess: updatedChain => {
        queryClient.setQueryData(chainKeys.byPath(vfsPath), updatedChain);
      },
    },
  );
}

export function useAddChainTask(vfsPath: string) {
  const queryClient = useQueryClient();
  return useMutation<ChainDefinition, Error, ChainTask, unknown>({
    mutationFn: async newTask => {
      const chain = await queryClient.fetchQuery({
        queryKey: chainKeys.byPath(vfsPath),
        queryFn: () => api.getChain(vfsPath),
      });

      return api.updateChain(vfsPath, {
        tasks: [...chain.tasks, newTask],
      });
    },
    onSuccess: updatedChain => {
      queryClient.setQueryData(chainKeys.byPath(vfsPath), updatedChain);
    },
  });
}

export function useRemoveChainTask(vfsPath: string) {
  const queryClient = useQueryClient();
  return useMutation<ChainDefinition, Error, string, unknown>({
    mutationFn: async taskId => {
      const chain = await queryClient.fetchQuery({
        queryKey: chainKeys.byPath(vfsPath),
        queryFn: () => api.getChain(vfsPath),
      });

      return api.updateChain(vfsPath, {
        tasks: chain.tasks.filter(task => task.id !== taskId),
      });
    },
    onSuccess: updatedChain => {
      queryClient.setQueryData(chainKeys.byPath(vfsPath), updatedChain);
    },
  });
}
