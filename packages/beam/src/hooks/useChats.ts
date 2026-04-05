import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../lib/api';
import { chatKeys } from '../lib/queryKeys';
import { ChatMessage, ChatSession, StateResponse } from '../lib/types';

export function useChats() {
  return useQuery<ChatSession[]>({
    queryKey: chatKeys.all,
    queryFn: api.getChats,
  });
}

export function useCreateChat() {
  const queryClient = useQueryClient();
  return useMutation<Partial<ChatSession>, Error, Partial<ChatSession>>({
    mutationFn: api.createChat,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: chatKeys.all });
    },
  });
}

export function useChatHistory(id: string, options?: { enabled?: boolean }) {
  return useQuery<ChatMessage[]>({
    queryKey: chatKeys.history(id),
    queryFn: () => api.getChatHistory(id),
    enabled: options?.enabled ?? !!id,
  });
}

export function useSendMessage(chatId: string) {
  const queryClient = useQueryClient();
  return useMutation<
    StateResponse,
    Error,
    {
      message: string;
      chainId: string;
      model?: string;
      provider?: string;
      signal?: AbortSignal;
      requestId?: string;
    }
  >({
    mutationFn: ({ message, chainId, model, provider, signal, requestId }) =>
      api.sendMessage(chatId, message, chainId, { model, provider, signal, requestId }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: chatKeys.history(chatId) });
    },
  });
}
