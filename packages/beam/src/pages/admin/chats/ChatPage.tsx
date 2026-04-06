import {
  Badge,
  Button,
  EmptyState,
  Panel,
  Section,
  Select,
  Span,
  Spinner,
  Tooltip,
} from '@contenox/ui';
import { PanelRightClose, PanelRightOpen } from 'lucide-react';
import { t } from 'i18next';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useLocation, useNavigate, useParams } from 'react-router-dom';
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
import { TaskEventFeed } from './components/TaskEventFeed';

const STATE_PANEL_STORAGE_KEY = 'beam_chat_state_panel_open';

type BeamChatLocationState = {
  beamInitialMessage?: string;
  beamInitialChainId?: string;
};

export default function ChatPage() {
  const { chatId: paramChatId } = useParams<{ chatId: string }>();
  const location = useLocation();
  const navigate = useNavigate();
  const [message, setMessage] = useState('');
  const [chatId, setChatId] = useState<string | null>(paramChatId || null);
  const [operationError, setOperationError] = useState<string | null>(null);
  const [selectedChainId, setSelectedChainId] = useState('');
  const [latestState, setLatestState] = useState<CapturedStateUnit[]>([]);
  const [isProcessing, setIsProcessing] = useState(false);
  const [activeRequestId, setActiveRequestId] = useState<string | null>(null);
  /** True after POST /api/chats/:id/chat has been dispatched for the current run. */
  const [httpDispatched, setHttpDispatched] = useState(false);
  const abortRef = useRef<AbortController | null>(null);
  const cancelledRef = useRef(false);
  const activeRequestIdRef = useRef<string | null>(null);
  const pendingSendRef = useRef<{
    requestId: string;
    message: string;
    chainId: string;
    signal: AbortSignal;
  } | null>(null);
  const sendDispatchedRef = useRef(false);
  const landingInitialSendKeyRef = useRef<string | null>(null);

  const [statePanelOpen, setStatePanelOpen] = useState(() => {
    if (typeof window === 'undefined') return true;
    return window.localStorage.getItem(STATE_PANEL_STORAGE_KEY) !== '0';
  });

  const toggleStatePanel = () => {
    setStatePanelOpen(open => {
      const next = !open;
      try {
        window.localStorage.setItem(STATE_PANEL_STORAGE_KEY, next ? '1' : '0');
      } catch {
        /* ignore quota / private mode */
      }
      return next;
    });
  };

  const { data: files = [], isLoading: chainsLoading } = useListFiles();
  const chainPaths = useMemo(
    () => files.filter(f => isChainLikeVfsPath(f.path)).map(f => f.path),
    [files],
  );

  useEffect(() => {
    if (paramChatId) setChatId(paramChatId);
  }, [paramChatId]);

  const { data: chatHistory, isLoading: historyLoading, error } = useChatHistory(chatId || '');
  const { mutate: sendMessage, error: sendError } = useSendMessage(chatId || '');

  const {
    mutate: createChat,
    isError,
    error: createError,
    isPending: isCreating,
  } = useCreateChat();

  const tryDispatchSend = useCallback(() => {
    const pending = pendingSendRef.current;
    if (!pending || sendDispatchedRef.current) return;
    if (pending.requestId !== activeRequestIdRef.current) return;
    sendDispatchedRef.current = true;
    setHttpDispatched(true);

    sendMessage(
      {
        message: pending.message,
        chainId: pending.chainId,
        signal: pending.signal,
        requestId: pending.requestId,
      },
      {
        onSuccess: response => {
          setLatestState(response.state || []);
          if (response.error) {
            setOperationError(response.error);
          }
          abortRef.current = null;
          setIsProcessing(false);
          setActiveRequestId(null);
          activeRequestIdRef.current = null;
          pendingSendRef.current = null;
          sendDispatchedRef.current = false;
          setHttpDispatched(false);
        },
        onError: (_, variables) => {
          abortRef.current = null;
          if (variables.signal?.aborted) {
            setOperationError(t('common.cancel', 'Cancel'));
          }
          setIsProcessing(false);
          setActiveRequestId(null);
          activeRequestIdRef.current = null;
          pendingSendRef.current = null;
          sendDispatchedRef.current = false;
          setHttpDispatched(false);
        },
      },
    );
  }, [sendMessage, t]);

  const { state: liveTask, connectionState: sseConnection } = useTaskEvents(activeRequestId, {
      enabled: !!activeRequestId && isProcessing,
      onConnectionOpen: () => {
        tryDispatchSend();
      },
    });

  /** If the event stream never reaches open, still send the chat request so the run can complete. */
  useEffect(() => {
    if (!activeRequestId || !isProcessing || sendDispatchedRef.current) return;
    const id = window.setTimeout(() => {
      tryDispatchSend();
    }, 4000);
    return () => window.clearTimeout(id);
  }, [activeRequestId, isProcessing, tryDispatchSend]);

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

  const processingBarLabel = useMemo(() => {
    if (!isProcessing) return '';
    if (!httpDispatched) {
      if (sseConnection === 'connecting') return t('chat.sse_connecting');
      if (sseConnection === 'open') return t('chat.sse_sending_http');
      if (sseConnection === 'error') return t('chat.sse_degraded');
      return t('chat.sse_queued');
    }
    if (liveTask.error) {
      return liveTask.status || t('chat.task_failed');
    }
    return liveTask.status || t('chat.thinking');
  }, [httpDispatched, isProcessing, liveTask.error, liveTask.status, sseConnection, t]);

  const submitOutgoingMessage = useCallback(
    (text: string, chainIdForSend: string) => {
      setOperationError(null);
      if (!text.trim()) return;

      if (!chainIdForSend) {
        setOperationError(t('chat.select_chain_first', 'Please select a task chain before sending.'));
        return;
      }

      abortRef.current?.abort();
      const controller = new AbortController();
      const requestId = createTaskEventRequestId();
      cancelledRef.current = false;
      abortRef.current = controller;

      pendingSendRef.current = {
        requestId,
        message: text.trim(),
        chainId: chainIdForSend,
        signal: controller.signal,
      };
      sendDispatchedRef.current = false;
      setHttpDispatched(false);
      activeRequestIdRef.current = requestId;
      setActiveRequestId(requestId);
      setIsProcessing(true);
      setMessage('');
    },
    [t],
  );

  const handleSendMessage = (e: React.FormEvent) => {
    e.preventDefault();
    submitOutgoingMessage(message, selectedChainId);
  };

  /** After `/chat` creates a session and navigates here with state, send the first message once. */
  useEffect(() => {
    if (!paramChatId) return;
    const st = location.state as BeamChatLocationState | null;
    if (!st?.beamInitialMessage?.trim() || !st.beamInitialChainId) return;

    const text = st.beamInitialMessage.trim();
    const chain = st.beamInitialChainId;
    const dedupeKey = `${paramChatId}\0${text}\0${chain}`;
    if (landingInitialSendKeyRef.current === dedupeKey) return;
    landingInitialSendKeyRef.current = dedupeKey;

    navigate({ pathname: location.pathname }, { replace: true, state: null });
    setSelectedChainId(chain);
    queueMicrotask(() => {
      submitOutgoingMessage(text, chain);
    });
  }, [paramChatId, location.state, location.pathname, navigate, submitOutgoingMessage]);

  const handleStop = () => {
    cancelledRef.current = true;
    abortRef.current?.abort();
    abortRef.current = null;
    pendingSendRef.current = null;
    sendDispatchedRef.current = false;
    activeRequestIdRef.current = null;
    setActiveRequestId(null);
    setIsProcessing(false);
    setHttpDispatched(false);
  };

  const handleCreateChat = () => createChat({});

  const chainOptions = [
    { value: '', label: t('chat.no_chain') },
    ...chainPaths.map(p => ({ value: p, label: p })),
  ];
  const displayHistory = useMemo<ApiChatMessage[]>(() => {
    const base = chatHistory ?? [];
    if (!isProcessing || !activeRequestId) {
      return base;
    }
    return [
      ...base,
      {
        id: `live-${activeRequestId}`,
        role: 'assistant',
        content: liveTask.content,
        sentAt: new Date().toISOString(),
        isUser: false,
        isLatest: true,
        streaming: true,
        error: liveTask.error ?? undefined,
      },
    ];
  }, [activeRequestId, chatHistory, isProcessing, liveTask.content, liveTask.error]);

  const streamScrollSignature = useMemo(
    () =>
      `${liveTask.content.length}\0${liveTask.thinking.length}\0${liveTask.events.length}\0${liveTask.status}`,
    [liveTask.content.length, liveTask.events.length, liveTask.status, liveTask.thinking.length],
  );

  return (
    <Page bodyScroll="hidden" className="h-full">
      <Fill className="flex">
        {/* Main Chat Area */}
        <div className="flex min-w-0 flex-1 flex-col">
          {/* Chat Content */}
          <Fill className="bg-surface-50 dark:bg-dark-surface-100 flex flex-col">
            {chatId ? (
              <>
                <div className="bg-surface-50 dark:bg-dark-surface-200 text-text dark:text-dark-text flex shrink-0 flex-wrap items-center gap-x-3 gap-y-2 border-b px-3 py-2">
                  <div className="flex min-w-0 flex-1 items-center gap-2 sm:gap-3">
                    <div className="flex shrink-0 items-center gap-1.5">
                      <Span variant="muted" className="text-xs sm:text-sm">
                        {t('chat.task_chain')}
                      </Span>
                      <Tooltip content={t('chat.chain_tooltip')} position="top">
                        <Badge variant="outline" size="sm" className="cursor-help px-1.5">
                          ?
                        </Badge>
                      </Tooltip>
                    </div>
                    <Select
                      options={chainOptions}
                      value={selectedChainId}
                      onChange={e => setSelectedChainId(e.target.value)}
                      className="min-w-[10rem] max-w-full flex-1 sm:max-w-md"
                      disabled={chainsLoading}
                    />
                    {chainsLoading && <Spinner size="sm" />}
                  </div>
                  <span
                    className="text-text-muted dark:text-dark-text-muted shrink-0 text-xs"
                    title={`${t('chat.messages')}: ${chatHistory?.length ?? 0}, ${t('chat.state_updates')}: ${latestState.length}`}>
                    {t('chat.stats_compact', {
                      messages: chatHistory?.length ?? 0,
                      state: latestState.length,
                    })}
                  </span>
                </div>

                <Fill className="relative">
                  {httpDispatched && sseConnection === 'error' && (
                    <div className="bg-warning-50 dark:bg-dark-surface-300 text-warning-900 dark:text-dark-text border-warning-200 dark:border-dark-surface-500 shrink-0 border-b px-3 py-1.5 text-xs">
                      {t('chat.sse_stream_lost')}
                    </div>
                  )}
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
                        processingBarLabel={processingBarLabel}
                        embedStreamThinkingInThread
                        liveThinking={liveTask.thinking}
                        canStop={isProcessing}
                        onStop={handleStop}
                        streamScrollSignature={streamScrollSignature}
                      />
                    )}
                  </div>
                </Fill>

                <Panel className="bg-surface-50 dark:bg-dark-surface-200 shrink-0 border-t">
                  <MessageInputForm
                    value={message}
                    onChange={setMessage}
                    onSubmit={handleSendMessage}
                    isPending={isProcessing}
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

        {/* Run log (task state): collapsible to reclaim horizontal space */}
        {statePanelOpen ? (
          <div className="bg-surface-50 dark:bg-dark-surface-200 flex w-[min(100%,20rem)] flex-shrink-0 flex-col border-l sm:w-80">
            <div className="flex shrink-0 items-center justify-between gap-2 border-b px-2 py-2">
              <div className="flex min-w-0 items-center gap-2">
                <Span variant="body" className="text-text dark:text-dark-text truncate font-medium">
                  {t('chat.run_log')}
                </Span>
                <Badge variant={latestState.length > 0 || liveTask.events.length > 0 ? 'success' : 'secondary'} size="sm">
                  {isProcessing ? liveTask.events.length : latestState.length}
                </Badge>
              </div>
              <Tooltip content={t('chat.hide_run_log')} position="left">
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  className="shrink-0"
                  onClick={toggleStatePanel}
                  aria-label={t('chat.hide_run_log')}>
                  <PanelRightClose className="h-4 w-4" />
                </Button>
              </Tooltip>
            </div>
            <Fill className="flex flex-col gap-2 overflow-auto p-2">
              {liveTask.events.length > 0 ? (
                <div className="shrink-0">
                  <Span variant="muted" className="mb-1 block text-xs font-medium">
                    {t('chat.task_events_feed_title')}
                  </Span>
                  <TaskEventFeed events={liveTask.events} />
                </div>
              ) : null}
              {latestState.length > 0 ? (
                <div className="min-h-0 flex-1 overflow-auto">
                  <Span variant="muted" className="mb-1 block text-xs font-medium">
                    {t('chat.captured_state_title')}
                  </Span>
                  <StateVisualizer state={latestState} />
                </div>
              ) : liveTask.events.length === 0 ? (
                <EmptyState
                  title={t('chat.no_state_data')}
                  description={t('chat.state_will_appear_here')}
                  icon="📊"
                  orientation="vertical"
                  iconSize="md"
                  className="text-text dark:text-dark-text-muted h-full"
                />
              ) : (
                <Span variant="muted" className="text-xs">
                  {t('chat.captured_state_pending')}
                </Span>
              )}
            </Fill>
          </div>
        ) : (
          <Tooltip content={t('chat.show_run_log')} position="left">
            <button
              type="button"
              onClick={toggleStatePanel}
              className="bg-surface-50 dark:bg-dark-surface-200 text-secondary-600 hover:bg-surface-100 dark:text-dark-secondary-400 dark:hover:bg-dark-surface-300 flex w-9 shrink-0 flex-col items-center justify-center border-l"
              aria-label={t('chat.show_run_log')}>
              <PanelRightOpen className="h-4 w-4" />
              {(isProcessing ? liveTask.events.length : latestState.length) > 0 ? (
                <Badge variant="success" size="sm" className="mt-1">
                  {isProcessing ? liveTask.events.length : latestState.length}
                </Badge>
              ) : null}
            </button>
          </Tooltip>
        )}
      </Fill>
    </Page>
  );
}
