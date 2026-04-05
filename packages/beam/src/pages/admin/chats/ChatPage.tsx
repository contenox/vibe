import {
  Badge,
  Button,
  EmptyState,
  H3,
  KeyValue,
  Panel,
  Section,
  Select,
  Span,
  Spinner,
  Tooltip,
} from '@contenox/ui';
import { t } from 'i18next';
import { useEffect, useMemo, useRef, useState } from 'react';
import { useParams } from 'react-router-dom';
import { Fill, Page } from '../../../components/Page';
import { useListFiles } from '../../../hooks/useFiles';
import { isChainLikeVfsPath } from '../../../lib/chainPaths';
import { useChatHistory, useCreateChat, useSendMessage } from '../../../hooks/useChats';
import { useTaskEvents } from '../../../hooks/useTaskEvents';
import { createTaskEventRequestId } from '../../../lib/taskEvents';
import { CapturedStateUnit, ChatMessage as ApiChatMessage } from '../../../lib/types';
import { ChatInterface } from './components/ChatInterface';
import { MessageInputForm } from './components/MessageInputForm';
import { StateVisualizer } from './components/StateVisualizer';

export default function ChatPage() {
  const { chatId: paramChatId } = useParams<{ chatId: string }>();
  const [message, setMessage] = useState('');
  const [chatId, setChatId] = useState<string | null>(paramChatId || null);
  const [operationError, setOperationError] = useState<string | null>(null);
  const [selectedChainId, setSelectedChainId] = useState('');
  const [latestState, setLatestState] = useState<CapturedStateUnit[]>([]);
  const [isProcessing, setIsProcessing] = useState(false);
  const [activeRequestId, setActiveRequestId] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  const cancelledRef = useRef(false);

  const { data: files = [], isLoading: chainsLoading } = useListFiles();
  const chainPaths = useMemo(
    () => files.filter(f => isChainLikeVfsPath(f.path)).map(f => f.path),
    [files],
  );

  useEffect(() => {
    if (paramChatId) setChatId(paramChatId);
  }, [paramChatId]);

  const { data: chatHistory, isLoading: historyLoading, error } = useChatHistory(chatId || '');
  const {
    mutate: sendMessage,
    isPending: isSending,
    error: sendError,
  } = useSendMessage(chatId || '');

  const {
    mutate: createChat,
    isError,
    error: createError,
    isPending: isCreating,
  } = useCreateChat();
  const liveTask = useTaskEvents(activeRequestId, { enabled: !!activeRequestId && isProcessing });

  useEffect(() => {
    const errorMessage = sendError?.message;
    if (errorMessage) {
      if (cancelledRef.current) {
        cancelledRef.current = false;
        return;
      }
      setOperationError(errorMessage);
      const timer = setTimeout(() => setOperationError(null), 8000);
      return () => clearTimeout(timer);
    }
  }, [sendError]);

  useEffect(() => {
    if (cancelledRef.current && isSending) {
      return;
    }
    setIsProcessing(isSending);
  }, [isSending]);

  const handleSendMessage = (e: React.FormEvent) => {
    e.preventDefault();
    setOperationError(null);
    if (!message.trim()) return;

    if (!selectedChainId) {
      setOperationError(t('chat.select_chain_first', 'Please select a task chain before sending.'));
      return;
    }

    abortRef.current?.abort();
    const controller = new AbortController();
    const requestId = createTaskEventRequestId();
    cancelledRef.current = false;
    abortRef.current = controller;
    setActiveRequestId(requestId);
    setIsProcessing(true);

    sendMessage(
      { message, chainId: selectedChainId, signal: controller.signal, requestId },
      {
        onSuccess: response => {
          setLatestState(response.state || []);
          if (response.error) {
            setOperationError(response.error);
          }
          abortRef.current = null;
          setIsProcessing(false);
        },
        onError: (_, variables) => {
          if (variables.signal?.aborted) {
            abortRef.current = null;
            setOperationError(t('common.cancel', 'Cancel'));
            return;
          }
          abortRef.current = null;
          setIsProcessing(false);
        },
      },
    );
    setMessage('');
  };

  const handleStop = () => {
    cancelledRef.current = true;
    abortRef.current?.abort();
    abortRef.current = null;
    setIsProcessing(false);
  };

  const handleCreateChat = () => createChat({});

  const chainOptions = [
    { value: '', label: t('chat.no_chain') },
    ...chainPaths.map(p => ({ value: p, label: p })),
  ];
  const displayHistory = useMemo<ApiChatMessage[]>(() => {
    const base = chatHistory ?? [];
    if (!isProcessing || !liveTask.content) {
      return base;
    }
    return [
      ...base,
      {
        id: `live-${activeRequestId ?? 'pending'}`,
        role: 'assistant',
        content: liveTask.content,
        sentAt: new Date().toISOString(),
        isUser: false,
        isLatest: true,
      },
    ];
  }, [activeRequestId, chatHistory, isProcessing, liveTask.content]);

  return (
    <Page bodyScroll="hidden" className="h-full">
      {/* Processing banner */}
      {isProcessing && (
        <div className="fixed top-4 left-1/2 z-50 -translate-x-1/2 transform">
          <Panel className="bg-surface-100 dark:bg-dark-surface-200 text-text dark:text-dark-text flex items-center gap-3 px-4 py-2 shadow-lg">
            <Spinner size="sm" />
            <Span variant="body" className="text-sm">
              {t('chat.processing')}
            </Span>
          </Panel>
        </div>
      )}

      <Fill className="flex">
        {/* Main Chat Area */}
        <div className="flex min-w-0 flex-1 flex-col">
          {/* Simple header (no system prompt here anymore) */}
          <Panel className="bg-surface-50 dark:bg-dark-surface-200 text-text dark:text-dark-text shrink-0 border-b">
            <div className="flex items-center justify-between p-4">
              <div className="flex items-center gap-3">
                <H3 className="text-text dark:text-dark-text">{t('chat.title')}</H3>
                {chatId && (
                  <Badge variant="primary" size="sm">
                    {t('chat.active')}
                  </Badge>
                )}
              </div>
            </div>
          </Panel>

          {/* Chat Content */}
          <Fill className="bg-surface-50 dark:bg-dark-surface-100 flex flex-col">
            {chatId ? (
              <>
                <Panel className="bg-surface-50 dark:bg-dark-surface-200 text-text dark:text-dark-text shrink-0">
                  <div className="flex items-center justify-between p-4">
                    <div className="flex items-center gap-4">
                      <div className="flex items-center gap-2">
                        <Span variant="body" className="font-medium">
                          {t('chat.task_chain')}
                        </Span>
                        <Tooltip content={t('chat.chain_tooltip')} position="top">
                          <Badge variant="outline" size="sm">
                            ?
                          </Badge>
                        </Tooltip>
                      </div>
                      <Select
                        options={chainOptions}
                        value={selectedChainId}
                        onChange={e => setSelectedChainId(e.target.value)}
                        className="w-64"
                        disabled={chainsLoading}
                      />
                      {chainsLoading && <Spinner size="sm" />}
                    </div>

                    <div className="flex items-center gap-4">
                      <KeyValue
                        label={t('chat.messages')}
                        value={chatHistory?.length || 0}
                        className="text-sm"
                      />
                      <KeyValue
                        label={t('chat.state_updates')}
                        value={latestState.length}
                        className="text-sm"
                      />
                    </div>
                  </div>
                </Panel>

                <Fill className="relative">
                  {operationError && (
                    <Panel className="bg-error-500 dark:bg-dark-error-100 text-error-800 dark:text-dark-text absolute top-4 right-4 left-4 z-10">
                      <div className="flex items-center justify-between">
                        <div className="flex items-center gap-2">
                          <Span variant="body">{operationError}</Span>
                        </div>
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => setOperationError(null)}
                          className="text-error dark:text-dark-text">
                          {t('common.dismiss')}
                        </Button>
                      </div>
                    </Panel>
                  )}

                  <div className="h-full overflow-auto">
                    {chatHistory && Array.isArray(chatHistory) && (
                      <ChatInterface
                        chatHistory={displayHistory}
                        isLoading={historyLoading}
                        error={error}
                        isProcessing={isProcessing}
                        liveThinking={liveTask.thinking}
                        liveStatus={liveTask.status}
                        canStop={isProcessing}
                        onStop={handleStop}
                      />
                    )}
                  </div>
                </Fill>

                <Panel className="bg-surface-50 dark:bg-dark-surface-200 shrink-0 border-t">
                  <MessageInputForm
                    value={message}
                    onChange={setMessage}
                    onSubmit={handleSendMessage}
                    isPending={isSending}
                    title={t('chat.chat_input')}
                    placeholder={t('chat.input_placeholder')}
                    canSubmit={!!selectedChainId}
                  />
                </Panel>
              </>
            ) : (
              <Section
                title={t('chat.no_chat_selected')}
                description={t('chat.start_conversation_prompt')}
                className="text-text dark:text-dark-text flex-1">
                <div className="mt-6 flex gap-3">
                  <Button onClick={handleCreateChat} size="lg" isLoading={isCreating}>
                    {t('chat.create_chat')}
                  </Button>
                  <Button variant="outline" size="lg">
                    {t('chat.view_examples')}
                  </Button>
                </div>
                {isError && (
                  <Panel className="bg-error-50 dark:bg-dark-error-400 text-error-800 dark:text-dark-text mt-4">
                    {createError?.message || t('chat.error_create_chat')}
                  </Panel>
                )}
              </Section>
            )}
          </Fill>
        </div>

        {/* Sidebar: State Visualizer */}
        <div className="bg-surface-50 dark:bg-dark-surface-200 flex w-96 flex-col border-l">
          {/* State Visualizer header */}
          <Panel className="shrink-0">
            <div className="flex items-center justify-between p-4">
              <H3 className="text-text dark:text-dark-text">{t('chat.state_visualizer')}</H3>
              <Badge variant={latestState.length > 0 ? 'success' : 'secondary'}>
                {latestState.length}
              </Badge>
            </div>
          </Panel>

          {/* State Visualizer content */}
          <Fill className="overflow-auto">
            {latestState.length > 0 ? (
              <StateVisualizer state={latestState} />
            ) : (
              <EmptyState
                title={t('chat.no_state_data')}
                description={t('chat.state_will_appear_here')}
                icon="📊"
                orientation="vertical"
                iconSize="md"
                className="text-text dark:text-dark-text-muted h-full"
              />
            )}
          </Fill>
        </div>
      </Fill>
    </Page>
  );
}
