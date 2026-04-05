import {
  useMutation,
  UseMutationResult,
  useQuery,
  useQueryClient,
  UseQueryResult,
} from '@tanstack/react-query';
import { api } from '../lib/api';
import { fileKeys, folderKeys } from '../lib/queryKeys';
import { FileResponse, FolderResponse } from '../lib/types';

export function useFileMetadata(id: string): UseQueryResult<FileResponse, Error> {
  return useQuery<FileResponse, Error>({
    queryKey: fileKeys.detail(id!),
    queryFn: () => api.getFileMetadata(id!),
  });
}

export function useCreateFile(): UseMutationResult<FileResponse, Error, FormData, unknown> {
  const queryClient = useQueryClient();
  return useMutation<FileResponse, Error, FormData>({
    mutationFn: api.createFile,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: fileKeys.lists() });
      queryClient.invalidateQueries({ queryKey: fileKeys.paths() });
    },
  });
}

export function useUpdateFile(): UseMutationResult<
  FileResponse,
  Error,
  { id: string; formData: FormData },
  unknown
> {
  const queryClient = useQueryClient();
  return useMutation<FileResponse, Error, { id: string; formData: FormData }>({
    mutationFn: ({ id, formData }) => api.updateFile(id, formData),
    onSuccess: (_, variables) => {
      queryClient.invalidateQueries({ queryKey: fileKeys.lists() });
      queryClient.invalidateQueries({ queryKey: fileKeys.detail(variables.id) });
      queryClient.invalidateQueries({ queryKey: fileKeys.paths() });
    },
  });
}

export function useDeleteFile(): UseMutationResult<void, Error, string, unknown> {
  const queryClient = useQueryClient();
  return useMutation<void, Error, string>({
    mutationFn: api.deleteFile,
    onSuccess: (_, deletedFileId) => {
      queryClient.invalidateQueries({ queryKey: fileKeys.lists() });
      queryClient.removeQueries({ queryKey: fileKeys.detail(deletedFileId) });
      queryClient.invalidateQueries({ queryKey: fileKeys.paths() });
    },
  });
}

export function useListFiles(path?: string): UseQueryResult<FileResponse[], Error> {
  return useQuery<FileResponse[], Error>({
    queryKey: [...fileKeys.lists(), { path }],
    queryFn: () => api.listFiles(path),
  });
}

export function useCreateFolder(): UseMutationResult<
  FolderResponse,
  Error,
  { path: string },
  unknown
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ path }) => api.createFolder({ path }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: fileKeys.paths() });
      queryClient.invalidateQueries({ queryKey: folderKeys.lists() });
    },
  });
}

export function useRenameFolder(): UseMutationResult<
  FolderResponse,
  Error,
  { id: string; path: string },
  unknown
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, path }) => api.renameFolder(id, { path }),
    onSuccess: (_, variables) => {
      queryClient.invalidateQueries({ queryKey: folderKeys.detail(variables.id) });
      queryClient.invalidateQueries({ queryKey: fileKeys.paths() });
      queryClient.invalidateQueries({ queryKey: folderKeys.lists() });
    },
  });
}

export function useRenameFile(): UseMutationResult<
  FileResponse,
  Error,
  { id: string; path: string },
  unknown
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, path }) => api.renameFile(id, { path }),
    onSuccess: (_, variables) => {
      queryClient.invalidateQueries({ queryKey: fileKeys.detail(variables.id) });
      queryClient.invalidateQueries({ queryKey: fileKeys.paths() });
    },
  });
}
