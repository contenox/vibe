import {
  Badge,
  Button,
  EmptyState,
  InlineNotice,
  InsetPanel,
  P,
  Section,
  Select,
  SidePanelBody,
  SidePanelColumn,
  SidePanelHeader,
  SidePanelRailButton,
  Span,
  Spinner,
  Tooltip,
} from '@contenox/ui';
import { FolderOpen, PanelRightClose, PanelRightOpen, X } from 'lucide-react';
import { t } from 'i18next';
import { useQueryClient } from '@tanstack/react-query';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useLocation, useNavigate, useParams } from 'react-router-dom';
import { Fill, Page } from '../../../components/Page';
import { useChain } from '../../../hooks/useChains';
import { useListFiles } from '../../../hooks/useFiles';
import { useActivePlan, useCompilePlanPreview } from '../../../hooks/usePlans';
import { isChainLikeVfsPath } from '../../../lib/chainPaths';
import { parseCompiledChainJSON } from '../../../lib/planCompiledChain';
import { planKeys } from '../../../lib/queryKeys';
import { useChatHistory, useCreateChat, useSendMessage } from '../../../hooks/useChats';
import { useTaskEvents } from '../../../hooks/useTaskEvents';
import { createTaskEventRequestId } from '../../../lib/taskEvents';
import { cn } from '../../../lib/utils';
import {
  CapturedStateUnit,
  ChatMessage as ApiChatMessage,
  type ChatContextPayload,
  type ChatModeId,
} from '../../../lib/types';
import { buildChatThreadItems } from './chatThreadItems';
import BuildModeChainGraph from './components/BuildModeChainGraph';
import { ChatInterface, type ChatWorkbenchTabId } from './components/ChatInterface';
import { CompiledPlanThreadEmbed } from './components/CompiledPlanThreadEmbed';
import { MessageInputForm } from './components/MessageInputForm';
import { StateVisualizer } from './components/StateVisualizer';
import { TaskEventFeed } from './components/TaskEventFeed';
import WorkspaceSplitPanel, { type WorkspaceSplitHandle } from './components/WorkspaceSplitPanel';

const STATE_PANEL_STORAGE_KEY = 'beam_chat_state_panel_open';
const WORKSPACE_PANEL_STORAGE_KEY = 'beam_chat_workspace_panel_open';
const WORKBENCH_TAB_STORAGE_KEY = 'beam_chat_workbench_tab';

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
  const [selectedMode, setSelectedMode] = useState<ChatModeId>('chat');
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
    mode: ChatModeId;
    signal: AbortSignal;
    context?: ChatContextPayload;
  } | null>(null);
  const sendDispatchedRef = useRef(false);
  const landingInitialSendKeyRef = useRef<string | null>(null);
  const workspaceRef = useRef<WorkspaceSplitHandle>(null);

  const [statePanelOpen, setStatePanelOpen] = useState(() => {
    if (typeof window === 'undefined') return true;
    return window.localStorage.getItem(STATE_PANEL_STORAGE_KEY) !== '0';
  });

  const [workbenchTab, setWorkbenchTab] = useState<ChatWorkbenchTabId>(() => {
    if (typeof window === 'undefined') return 'chat';
    return window.localStorage.getItem(WORKBENCH_TAB_STORAGE_KEY) === 'chain' ? 'chain' : 'chat';
  });

  const [workspacePanelOpen, setWorkspacePanelOpen] = useState(() => {
    if (typeof window === 'undefined') return true;
    return window.localStorage.getItem(WORKSPACE_PANEL_STORAGE_KEY) !== '0';
  });
  const [mobileWorkspaceOpen, setMobileWorkspaceOpen] = useState(false);

  const persistWorkspacePanelOpen = useCallback((open: boolean) => {
    setWorkspacePanelOpen(open);
    try {
      window.localStorage.setItem(WORKSPACE_PANEL_STORAGE_KEY, open ? '1' : '0');
    } catch {
      /* ignore */
    }
  }, []);

  const persistWorkbenchTab = useCallback((tab: ChatWorkbenchTabId) => {
    setWorkbenchTab(tab);
    try {
      window.localStorage.setItem(WORKBENCH_TAB_STORAGE_KEY, tab);
    } catch {
      /* ignore */
    }
  }, []);

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

  const queryClient = useQueryClient();
  const { data: chatHistory, isLoading: historyLoading, error } = useChatHistory(chatId || '');
  const { mutate: sendMessage, error: sendError } = useSendMessage(chatId || '');

  const { data: activePlan, isLoading: activePlanLoading } = useActivePlan({
    enabled: selectedMode === 'plan' && !!chatId,
  });
  const chainPathForExecutorPreview =
    chatId && selectedChainId.trim() ? selectedChainId : '';
  const {
    data: executorChainPreview,
    isLoading: executorChainLoading,
    error: executorChainError,
  } = useChain(chainPathForExecutorPreview);

  const compiledChainFromPlan = useMemo(
    () => parseCompiledChainJSON(activePlan?.plan.compiled_chain_json),
    [activePlan?.plan.compiled_chain_json],
  );

  const compilePlanPreview = useCompilePlanPreview({
    enabled:
      selectedMode === 'plan' &&
      !!chatId &&
      !!selectedChainId.trim() &&
      !!activePlan?.steps?.length &&
      !compiledChainFromPlan,
    plan: activePlan?.plan,
    steps: activePlan?.steps,
    executorChainId: selectedChainId,
  });

  const compiledWorkflowChain = useMemo(
    () => compiledChainFromPlan ?? compilePlanPreview.data?.chain ?? null,
    [compiledChainFromPlan, compilePlanPreview.data?.chain],
  );

  const executorGraphCaption = useMemo(() => {
    if (!selectedChainId.trim()) return null;
    return selectedMode === 'plan'
      ? t('chat.build_graph_executor_caption')
      : t('chat.chain_executor_caption');
  }, [selectedChainId, selectedMode, t]);

  const compiledGraphCaption = useMemo(() => {
    if (!compiledChainFromPlan && compilePlanPreview.data?.chain) {
      return t('chat.build_compiled_preview_note');
    }
    return null;
  }, [compiledChainFromPlan, compilePlanPreview.data?.chain, t]);

  const compiledGraphLoading =
    !!selectedChainId.trim() &&
    !!activePlan?.steps?.length &&
    !compiledChainFromPlan &&
    compilePlanPreview.isLoading;

  const compiledGraphError =
    !compiledChainFromPlan && compilePlanPreview.error instanceof Error
      ? compilePlanPreview.error
      : null;

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
        mode: pending.mode,
        signal: pending.signal,
        requestId: pending.requestId,
        context: pending.context,
      },
      {
        onSuccess: response => {
          const wasBuild = pendingSendRef.current?.mode === 'build';
          setLatestState(response.state || []);
          if (response.error) {
            setOperationError(response.error);
          }
          if (wasBuild) {
            void queryClient.invalidateQueries({ queryKey: planKeys.active() });
            void queryClient.invalidateQueries({ queryKey: planKeys.compilePreviewPrefix() });
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
  }, [queryClient, sendMessage, t]);

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
    (text: string, chainIdForSend: string, modeForSend: ChatModeId) => {
      setOperationError(null);
      if (modeForSend !== 'build' && !text.trim()) return;
      if (modeForSend === 'build' && !chainIdForSend.trim()) {
        setOperationError(t('chat.build_chain_required'));
        return;
      }

      abortRef.current?.abort();
      const controller = new AbortController();
      const requestId = createTaskEventRequestId();
      cancelledRef.current = false;
      abortRef.current = controller;

      const context = workspaceRef.current?.buildChatContext();

      pendingSendRef.current = {
        requestId,
        message: text.trim(),
        chainId: chainIdForSend.trim(),
        mode: modeForSend,
        signal: controller.signal,
        context,
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
    submitOutgoingMessage(message, selectedChainId, selectedMode);
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
      submitOutgoingMessage(text, chain, 'chat');
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
  const modeOptions: { value: ChatModeId; label: string }[] = [
    { value: 'chat', label: t('chat.mode_chat') },
    { value: 'prompt', label: t('chat.mode_prompt') },
    { value: 'plan', label: t('chat.mode_plan') },
    { value: 'build', label: t('chat.mode_build') },
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

  const compiledPlanEmbedKey = useMemo(
    () =>
      `${activePlan?.plan?.id ?? 'noplan'}-${compiledWorkflowChain?.id ?? 'nochain'}-${compiledGraphLoading}-${compilePlanPreview.isPending}`,
    [
      activePlan?.plan?.id,
      compiledWorkflowChain?.id,
      compiledGraphLoading,
      compilePlanPreview.isPending,
    ],
  );

  const threadItems = useMemo(
    () =>
      buildChatThreadItems({
        displayHistory,
        insertCompiledPlanEmbed: selectedMode === 'plan' && !!chatId,
        embedKey: compiledPlanEmbedKey,
      }),
    [displayHistory, selectedMode, chatId, compiledPlanEmbedKey],
  );

  const compiledPlanEmbedContent = useMemo(
    () => (
      <CompiledPlanThreadEmbed
        chain={compiledWorkflowChain}
        caption={compiledGraphCaption}
        isLoading={compiledGraphLoading}
        error={compiledGraphError}
        activePlanLoading={activePlanLoading}
        hasActivePlan={!!activePlan}
        hasSteps={!!activePlan?.steps?.length}
      />
    ),
    [
      compiledWorkflowChain,
      compiledGraphCaption,
      compiledGraphLoading,
      compiledGraphError,
      activePlanLoading,
      activePlan,
    ],
  );

  const chainPanel = useMemo(
    () =>
      !selectedChainId.trim() ? (
        <P className="text-text-secondary dark:text-dark-text-muted p-3 text-sm">
          {t('chat.build_graph_select_chain')}
        </P>
      ) : (
        <BuildModeChainGraph
          chain={
            executorChainPreview ?? {
              id: 'executor-preview',
              description: '',
              tasks: [],
            }
          }
          caption={executorGraphCaption}
          isLoading={executorChainLoading}
          error={executorChainError instanceof Error ? executorChainError : null}
        />
      ),
    [
      selectedChainId,
      executorChainPreview,
      executorGraphCaption,
      executorChainLoading,
      executorChainError,
      t,
    ],
  );

  const streamScrollSignature = useMemo(
    () =>
      `${liveTask.content.length}\0${liveTask.thinking.length}\0${liveTask.events.length}\0${liveTask.status}\0${workbenchTab}\0${threadItems.length}`,
    [
      liveTask.content.length,
      liveTask.events.length,
      liveTask.status,
      liveTask.thinking.length,
      workbenchTab,
      threadItems.length,
    ],
  );

  return (
    <Page bodyScroll="hidden" className="h-full">
      <Fill className="flex min-h-0">
        <div className="flex min-h-0 min-w-0 flex-1 flex-row">
          {/* Main Chat Area */}
          <div className="flex min-h-0 min-w-0 flex-1 flex-col">
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
                    <div className="flex shrink-0 items-center gap-1.5">
                      <Span variant="muted" className="text-xs sm:text-sm">
                        {t('chat.mode')}
                      </Span>
                      <Tooltip content={t('chat.mode_tooltip')} position="top">
                        <Badge variant="outline" size="sm" className="cursor-help px-1.5">
                          ?
                        </Badge>
                      </Tooltip>
                    </div>
                    <Select
                      options={modeOptions}
                      value={selectedMode}
                      onChange={e => setSelectedMode(e.target.value as ChatModeId)}
                      className="min-w-[7rem] max-w-[12rem] shrink-0"
                      disabled={isProcessing}
                    />
                  </div>
                  <span
                    className="text-text-muted dark:text-dark-text-muted shrink-0 text-xs"
                    title={`${t('chat.messages')}: ${chatHistory?.length ?? 0}, ${t('chat.state_updates')}: ${latestState.length}`}>
                    {t('chat.stats_compact', {
                      messages: chatHistory?.length ?? 0,
                      state: latestState.length,
                    })}
                  </span>
                  <div className="flex shrink-0 items-center gap-1">
                    <Tooltip content={t('chat.workspace_toggle_tooltip')}>
                      <Button
                        type="button"
                        variant={workspacePanelOpen ? 'secondary' : 'outline'}
                        size="sm"
                        className="shrink-0"
                        onClick={() => persistWorkspacePanelOpen(!workspacePanelOpen)}
                        aria-pressed={workspacePanelOpen}
                        aria-label={t('chat.workspace_toggle_aria')}>
                        <FolderOpen className="h-4 w-4" />
                      </Button>
                    </Tooltip>
                    {workspacePanelOpen ? (
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        className="lg:hidden"
                        onClick={() => setMobileWorkspaceOpen(true)}>
                        {t('chat.workspace_open_mobile')}
                      </Button>
                    ) : null}
                  </div>
                </div>

                {selectedMode === 'build' && (
                  <InsetPanel
                    tone="strip"
                    role="region"
                    aria-label={t('chat.build_graph_aria_panel')}>
                    <div className="flex shrink-0 items-center justify-between gap-2 px-3 py-2">
                      <Span variant="body" className="text-sm font-medium">
                        {t('chat.build_workflow_title')}
                      </Span>
                      <Button
                        type="button"
                        variant="primary"
                        size="sm"
                        disabled={isProcessing || !selectedChainId.trim()}
                        onClick={() => submitOutgoingMessage('', selectedChainId, 'build')}>
                        {t('chat.build_run_button')}
                      </Button>
                    </div>
                  </InsetPanel>
                )}

                <Fill className="relative">
                  {httpDispatched && sseConnection === 'error' && (
                    <InlineNotice variant="warning">{t('chat.sse_stream_lost')}</InlineNotice>
                  )}
                  {operationError && (
                    <InlineNotice variant="error">
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
                    </InlineNotice>
                  )}

                  <div className="h-full overflow-auto">
                    {chatHistory && Array.isArray(chatHistory) && (
                      <ChatInterface
                        threadItems={threadItems}
                        compiledPlanEmbedContent={compiledPlanEmbedContent}
                        workbenchTab={workbenchTab}
                        onWorkbenchTabChange={persistWorkbenchTab}
                        showWorkbenchTabs={!!selectedChainId.trim()}
                        chainPanel={chainPanel}
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

                <div className="bg-surface-50 dark:bg-dark-surface-200 shrink-0">
                  <MessageInputForm
                    value={message}
                    onChange={setMessage}
                    onSubmit={handleSendMessage}
                    isPending={isProcessing}
                    variant="workbench"
                    placeholder={
                      selectedMode === 'build'
                        ? t('chat.build_placeholder')
                        : t('chat.workbench_placeholder')
                    }
                    buttonLabel={t('chat.run_button')}
                    canSubmit={
                      !isProcessing &&
                      (selectedMode === 'build'
                        ? !!selectedChainId.trim()
                        : !!message.trim())
                    }
                    allowEmptyMessage={selectedMode === 'build'}
                  />
                </div>
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
                  <InlineNotice variant="errorSoft" className="mt-4">
                    {createError?.message || t('chat.error_create_chat')}
                  </InlineNotice>
                )}
              </Section>
            )}
          </Fill>
          </div>

          {chatId && workspacePanelOpen ? (
            <div
              className={cn(
                'border-border bg-surface-50 dark:bg-dark-surface-100 flex min-h-0 w-full shrink-0 flex-col border-l',
                mobileWorkspaceOpen
                  ? 'fixed inset-0 z-50 flex flex-col lg:static lg:inset-auto lg:z-auto lg:w-[min(420px,38vw)]'
                  : 'hidden lg:flex lg:w-[min(420px,38vw)]',
              )}>
              {mobileWorkspaceOpen ? (
                <div className="flex items-center justify-end gap-2 border-b px-2 py-1.5 lg:hidden">
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    onClick={() => setMobileWorkspaceOpen(false)}
                    aria-label={t('chat.workspace_close_mobile')}>
                    <X className="h-4 w-4" />
                  </Button>
                </div>
              ) : null}
              <WorkspaceSplitPanel ref={workspaceRef} className="min-h-0 flex-1 border-0" />
            </div>
          ) : null}
        </div>

        {/* Run log (task state): collapsible to reclaim horizontal space */}
        {statePanelOpen ? (
          <SidePanelColumn>
            <SidePanelHeader>
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
            </SidePanelHeader>
            <SidePanelBody>
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
            </SidePanelBody>
          </SidePanelColumn>
        ) : (
          <Tooltip content={t('chat.show_run_log')} position="left">
            <SidePanelRailButton onClick={toggleStatePanel} aria-label={t('chat.show_run_log')}>
              <PanelRightOpen className="h-4 w-4" />
              {(isProcessing ? liveTask.events.length : latestState.length) > 0 ? (
                <Badge variant="success" size="sm" className="mt-1">
                  {isProcessing ? liveTask.events.length : latestState.length}
                </Badge>
              ) : null}
            </SidePanelRailButton>
          </Tooltip>
        )}
      </Fill>
    </Page>
  );
}
