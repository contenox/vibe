import {
  Button,
  InlineNotice,
  P,
  ResizablePanel,
  ResizablePanelGroup,
  ResizablePanelHandle,
  Section,
  Span,
  Fill,
  Page,
} from '@contenox/ui';
import { X } from 'lucide-react';
import { t } from 'i18next';
import { useQueryClient } from '@tanstack/react-query';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useLocation, useNavigate, useParams } from 'react-router-dom';
import { useChain, useUpdateChain } from '../../../hooks/useChains';
import { useListPolicies, useSetActivePolicy } from '../../../hooks/usePolicies';
import { useSetupStatus } from '../../../hooks/useSetupStatus';
import { useListFiles } from '../../../hooks/useFiles';
import { useActivePlan, useCompilePlanPreview } from '../../../hooks/usePlans';
import { isChainLikeVfsPath } from '../../../lib/chainPaths';
import { parseCompiledChainJSON } from '../../../lib/planCompiledChain';
import { planKeys } from '../../../lib/queryKeys';
import { api } from '../../../lib/api';
import { useChatHistory, useCreateChat, useSendMessage } from '../../../hooks/useChats';
import { useTaskEvents } from '../../../hooks/useTaskEvents';
import { createTaskEventRequestId } from '../../../lib/taskEvents';
import { BEAM_LAYOUT_CHANGED_EVENT } from '../../../lib/beamLayout';
import { cn } from '../../../lib/utils';
import {
  CapturedStateUnit,
  ChatMessage as ApiChatMessage,
  type ChatContextArtifact,
  type ChatContextPayload,
  type ChatModeId,
  type InlineAttachment,
} from '../../../lib/types';
import { artifactsToInlineAttachments } from '../../../lib/inlineAttachments';
import {
  ArtifactRegistryProvider,
  useArtifactRegistry,
} from '../../../lib/artifacts';
import {
  SlashCommandRegistryProvider,
  createFileCommand,
  createHelpCommand,
  createPlanCommand,
  useSlashCommand,
  useSlashCommandRegistry,
  type FileResolver,
  type PlanProvider,
} from '../../../lib/slashCommands';
import {
  readWorkspaceFileText,
  MAX_EDITOR_FILE_BYTES,
} from '../../../lib/workspaceFileContext';
import { buildChatThreadItems } from './chatThreadItems';
import BuildModeChainGraph from './components/BuildModeChainGraph';
import { ChatInterface, type ChatWorkbenchTabId } from './components/ChatInterface';
import { CompiledPlanThreadEmbed } from './components/CompiledPlanThreadEmbed';
import { ApprovalCard } from './components/ApprovalCard';
import { MessageInputForm } from './components/MessageInputForm';
import WorkspaceSplitPanel, { type WorkspaceSplitHandle } from './components/WorkspaceSplitPanel';
import { ChatToolbar } from './components/ChatToolbar';
import { BuildModeStrip } from './components/BuildModeStrip';
import { ChatRunLog } from './components/ChatRunLog';

const STATE_PANEL_STORAGE_KEY = 'beam_chat_state_panel_open';
const WORKSPACE_PANEL_STORAGE_KEY = 'beam_chat_workspace_panel_open';
const WORKBENCH_TAB_STORAGE_KEY = 'beam_chat_workbench_tab';
const WORKSPACE_SPLIT_STORAGE_KEY = 'beam_chat_workspace_split_px';

function useIsMinLg(): boolean {
  const [lg, setLg] = useState(() =>
    typeof window !== 'undefined' ? window.matchMedia('(min-width: 1024px)').matches : false,
  );
  useEffect(() => {
    const mq = window.matchMedia('(min-width: 1024px)');
    const fn = () => setLg(mq.matches);
    mq.addEventListener('change', fn);
    return () => mq.removeEventListener('change', fn);
  }, []);
  return lg;
}

type BeamChatLocationState = {
  beamInitialMessage?: string;
  beamInitialChainId?: string;
};

/**
 * Thin wrapper that establishes the ArtifactRegistry for this ChatPage
 * instance, then renders the real component. The registry lives at this
 * boundary so descendants (files tree, terminal, composer, etc.) can all
 * register sources whose artifacts the composer collects at send time.
 */
export default function ChatPage() {
  return (
    <ArtifactRegistryProvider>
      <SlashCommandRegistryProvider>
        <ChatPageImpl />
      </SlashCommandRegistryProvider>
    </ArtifactRegistryProvider>
  );
}

/**
 * Merge the legacy workspace-derived context (today: file_excerpt built by
 * WorkspaceSplitPanel.buildChatContext) with artifacts contributed by sources
 * registered in the [ArtifactRegistry]. Registry sources appear AFTER the
 * workspace artifact so the LLM sees them as supplementary, not overriding.
 *
 * Returns undefined when there is nothing to attach — keeping the send path
 * byte-for-byte identical to the pre-registry behaviour when no sources are
 * active.
 */
function buildTurnContext(
  workspaceContext: ChatContextPayload | undefined,
  registryArtifacts: ChatContextArtifact[],
): ChatContextPayload | undefined {
  const artifacts: ChatContextArtifact[] = [];
  if (workspaceContext?.artifacts?.length) {
    artifacts.push(...workspaceContext.artifacts);
  }
  if (registryArtifacts.length) {
    artifacts.push(...registryArtifacts);
  }
  if (artifacts.length === 0) return undefined;
  return { artifacts };
}

/**
 * Optimistic user message awaiting echo from the persisted history. Carries
 * inline attachments derived from slash-armed artifact sources so the user
 * can see what they attached on this turn. Cleared once the persisted version
 * arrives (matched by content + sentAt windowing).
 */
type OptimisticUserOutgoing = {
  /** Synthetic id; not the eventual server id. */
  id: string;
  content: string;
  attachments: InlineAttachment[];
  sentAt: string;
};

/**
 * Convert a tool call (function name + JSON arguments string + result content)
 * into an inline attachment for rendering in the chat thread. Returns null for
 * tool calls that have no meaningful visual form (e.g. write_file).
 */
function toolCallToInlineAttachment(
  name: string,
  argsJson: string,
  content: string,
): InlineAttachment | null {
  try {
    const args = JSON.parse(argsJson) as Record<string, unknown>;
    switch (name) {
      case 'read_file':
      case 'read_file_range':
        return {
          kind: 'file_view',
          path: String(args.path ?? args.file ?? ''),
          text: content,
          truncated: false,
        };
      case 'grep':
        return {
          kind: 'terminal_excerpt',
          command: `grep ${String(args.pattern ?? '')} ${String(args.path ?? '')}`.trim(),
          output: content,
        };
      case 'list_dir':
        return {
          kind: 'terminal_excerpt',
          command: `ls ${String(args.path ?? '')}`.trim(),
          output: content,
        };
      case 'stat_file':
      case 'count_stats':
        return {
          kind: 'terminal_excerpt',
          command: `${name} ${String(args.path ?? '')}`.trim(),
          output: content,
        };
      default:
        return null;
    }
  } catch {
    return null;
  }
}

function ChatPageImpl() {
  const { chatId: paramChatId } = useParams<{ chatId: string }>();
  const artifactRegistry = useArtifactRegistry();
  const slashRegistry = useSlashCommandRegistry();
  const [optimisticOutgoing, setOptimisticOutgoing] = useState<OptimisticUserOutgoing | null>(
    null,
  );
  /**
   * Agent-emitted inline attachments keyed by persisted assistant message id
   * (Phase 5 of the canvas-vision plan). Captured from the live SSE stream
   * once the persisted echo arrives so attachments survive after the live
   * row collapses. Cleared on chat session change.
   */
  const [agentAttachments, setAgentAttachments] = useState<Record<string, InlineAttachment[]>>(
    {},
  );
  const location = useLocation();
  const navigate = useNavigate();
  const chatId = paramChatId ?? null;
  const [message, setMessage] = useState('');
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
  const lastFailedSendRef = useRef<{ text: string; chainId: string; mode: ChatModeId } | null>(null);
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
  const workspacePanelRef = useRef<HTMLDivElement | null>(null);
  const isLg = useIsMinLg();

  const workspaceSplitInitialPx = useMemo(() => {
    if (typeof window === 'undefined') return null;
    try {
      const raw = window.localStorage.getItem(WORKSPACE_SPLIT_STORAGE_KEY);
      const n = raw ? parseInt(raw, 10) : NaN;
      return Number.isFinite(n) && n >= 260 && n <= 900 ? n : null;
    } catch {
      return null;
    }
  }, []);

  const persistChatWorkspaceSplit = useCallback(() => {
    const el = workspacePanelRef.current;
    if (!el) return;
    const w = Math.round(el.getBoundingClientRect().width);
    if (w < 260 || w > 900) return;
    try {
      window.localStorage.setItem(WORKSPACE_SPLIT_STORAGE_KEY, String(w));
    } catch {
      /* ignore */
    }
  }, []);

  const onWorkspaceSplitResizeEnd = useCallback(() => {
    persistChatWorkspaceSplit();
    window.dispatchEvent(new CustomEvent(BEAM_LAYOUT_CHANGED_EVENT));
  }, [persistChatWorkspaceSplit]);

  const [statePanelOpen, setStatePanelOpen] = useState(() => {
    if (typeof window === 'undefined') return true;
    return window.localStorage.getItem(STATE_PANEL_STORAGE_KEY) !== '0';
  });

  const [workbenchTab, setWorkbenchTab] = useState<ChatWorkbenchTabId>(() => {
    if (typeof window === 'undefined') return 'chat';
    const stored = window.localStorage.getItem(WORKBENCH_TAB_STORAGE_KEY);
    if (stored === 'chain' || stored === 'plan') return stored;
    return 'chat';
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

  const { data: files = [], isLoading: chainsLoading } = useListFiles('.contenox');
  const chainPaths = useMemo(
    () => files.filter(f => isChainLikeVfsPath(f.path)).map(f => f.path.split('/').pop()!),
    [files],
  );

  // Preselect default-chain.json (or the first available chain) when no chain
  // has been chosen yet. Mirrors what the server does via chain_resolve.go.
  useEffect(() => {
    if (selectedChainId || !chainPaths.length) return;
    const def = chainPaths.find(p => p === 'default-chain.json') ?? chainPaths[0];
    setSelectedChainId(def);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [chainPaths]);

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
  const { mutate: updateChain } = useUpdateChain(chainPathForExecutorPreview);

  const { data: policyNames = [] } = useListPolicies();
  const { data: setupStatus } = useSetupStatus(true);
  const activePolicyName = setupStatus?.hitlPolicyName ?? '';
  const setActivePolicy = useSetActivePolicy();

  const compiledChainFromPlan = useMemo(
    () => parseCompiledChainJSON(activePlan?.plan.compiled_chain_json),
    [activePlan?.plan.compiled_chain_json],
  );

  /**
   * Slash command resolver: maps a user-supplied path to the VFS file +
   * downloaded text. Stable identity (no React state captured) so the
   * `/file` command object is memo-stable.
   */
  const fileResolver = useMemo<FileResolver>(
    () => ({
      resolve: async (input, signal) => {
        const needle = input.trim().replace(/^\/+/, '').toLowerCase();
        if (!needle) throw new Error('path required');
        // Search the entire VFS root once. Beam's lists are typically small.
        const all = await api.listFiles();
        const candidates = all.filter((f) => !f.isDirectory);
        const exact = candidates.filter(
          (f) => f.path.toLowerCase() === needle || f.id.toLowerCase() === needle,
        );
        const suffix = exact.length
          ? exact
          : candidates.filter((f) => f.path.toLowerCase().endsWith(needle));
        if (suffix.length === 0) {
          throw new Error(`no file matched "${input}"`);
        }
        if (suffix.length > 1) {
          const preview = suffix.slice(0, 5).map((f) => f.path).join(', ');
          throw new Error(`ambiguous (${suffix.length} matches): ${preview}…`);
        }
        const f = suffix[0]!;
        if (f.size > MAX_EDITOR_FILE_BYTES) {
          throw new Error(`${f.path} too large to attach`);
        }
        const text = await readWorkspaceFileText(f.id, signal);
        return { path: f.path, text };
      },
    }),
    [],
  );

  /**
   * `/plan` provider: pulls the active plan + steps fresh on every command
   * invocation so the artifact reflects the latest server state, not whatever
   * React Query happened to have cached.
   */
  const planProvider = useMemo<PlanProvider>(
    () => ({
      getActivePlanSteps: () => {
        // useActivePlan is gated by selectedMode === 'plan'; in any other mode
        // it returns undefined. The slash-command path fetches independently
        // by reading from React Query's cached snapshot when available.
        if (!activePlan?.plan?.id || !activePlan.steps?.length) return null;
        return {
          planId: activePlan.plan.id,
          steps: activePlan.steps.map((s) => ({
            ordinal: s.ordinal,
            description: s.description,
            status: s.status,
            summary: s.summary,
            failureClass: s.failure_class,
            lastFailure: s.last_failure_summary,
          })),
        };
      },
    }),
    [activePlan],
  );

  // Register built-in slash commands. The /help command reads the live
  // registry, so commands added later (e.g. /terminal from TerminalPanel)
  // are discoverable in the help output as soon as they register.
  const helpCommand = useMemo(() => createHelpCommand(slashRegistry), [slashRegistry]);
  const fileCommand = useMemo(() => createFileCommand(fileResolver), [fileResolver]);
  const planCommand = useMemo(() => createPlanCommand(planProvider), [planProvider]);
  useSlashCommand(helpCommand);
  useSlashCommand(fileCommand);
  useSlashCommand(planCommand);

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
            lastFailedSendRef.current = pendingSendRef.current
              ? { text: pendingSendRef.current.message, chainId: pendingSendRef.current.chainId, mode: pendingSendRef.current.mode }
              : null;
            setOperationError(response.error);
          } else {
            lastFailedSendRef.current = null;
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
            lastFailedSendRef.current = null;
            setOperationError(t('common.cancel', 'Cancel'));
          } else {
            lastFailedSendRef.current = pendingSendRef.current
              ? { text: pendingSendRef.current.message, chainId: pendingSendRef.current.chainId, mode: pendingSendRef.current.mode }
              : null;
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
        lastFailedSendRef.current = null;
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

      // Collect the registry exactly once: one-shot slash sources unregister
      // themselves on collect, so a second pass would yield nothing.
      const collected = artifactRegistry.collectWithSources();
      const allArtifacts = collected.map((p) => p.artifact);
      // Inline-display attachments come ONLY from explicitly-armed sources
      // (`mention:` from @-mentions, `slash:` legacy). Sticky sources
      // (workspace open_file, terminal armed_output) already have indicators
      // in their owning panels; rendering them on every user message would
      // be visual noise.
      const explicitArtifacts = collected
        .filter((p) => p.source.id.startsWith('mention:') || p.source.id.startsWith('slash:'))
        .map((p) => p.artifact);
      const inlineAttachments = artifactsToInlineAttachments(explicitArtifacts);

      const context = buildTurnContext(
        workspaceRef.current?.buildChatContext(),
        allArtifacts,
      );

      const trimmed = text.trim();
      // Only show optimistic user message when there's something to show —
      // empty body in build mode shouldn't manifest a blank bubble.
      if (trimmed || inlineAttachments.length > 0) {
        setOptimisticOutgoing({
          id: `optimistic-${requestId}`,
          content: trimmed,
          attachments: inlineAttachments,
          sentAt: new Date().toISOString(),
        });
      }

      pendingSendRef.current = {
        requestId,
        message: trimmed,
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
    [artifactRegistry, t],
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

    // Build a lookup from toolCallId → function metadata so tool-result
    // messages can be rendered as typed cards rather than raw text blobs.
    const callMap = new Map<string, { name: string; arguments: string }>();
    for (const m of base) {
      if (m.role === 'assistant' && m.callTools) {
        for (const tc of m.callTools) {
          callMap.set(tc.id, tc.function);
        }
      }
    }

    // Augment persisted assistant messages with their agent-emitted
    // attachments, when we captured any during their streaming turn.
    // Augment tool-result messages with typed inline attachments derived
    // from the function name + arguments of their paired call.
    const merged: ApiChatMessage[] = base.map((m) => {
      if (m.role === 'assistant' && m.id && agentAttachments[m.id]) {
        return { ...m, attachments: agentAttachments[m.id] };
      }
      if (m.role === 'tool' && m.toolCallId) {
        const fn = callMap.get(m.toolCallId);
        if (fn) {
          const attachment = toolCallToInlineAttachment(fn.name, fn.arguments, m.content);
          if (attachment) {
            return { ...m, attachments: [attachment] };
          }
        }
      }
      return m;
    });

    // Optimistic user message: appended only when its content has not yet
    // appeared in the persisted history. We match by content + a 5-minute
    // sentAt window to tolerate clock skew between client and server.
    if (optimisticOutgoing) {
      const optAt = Date.parse(optimisticOutgoing.sentAt);
      const matched = base.some((m) => {
        if (m.role !== 'user') return false;
        if (m.content !== optimisticOutgoing.content) return false;
        const persistedAt = Date.parse(m.sentAt);
        return Math.abs(persistedAt - optAt) < 5 * 60_000;
      });
      if (!matched) {
        merged.push({
          id: optimisticOutgoing.id,
          role: 'user',
          content: optimisticOutgoing.content,
          sentAt: optimisticOutgoing.sentAt,
          isUser: true,
          isLatest: true,
          attachments: optimisticOutgoing.attachments,
        });
      }
    }

    if (!isProcessing || !activeRequestId) {
      return merged;
    }
    merged.push({
      id: `live-${activeRequestId}`,
      role: 'assistant',
      content: liveTask.content,
      sentAt: new Date().toISOString(),
      isUser: false,
      isLatest: true,
      streaming: true,
      error: liveTask.error ?? undefined,
      attachments: liveTask.attachments,
      events: liveTask.events,
    });
    return merged;
  }, [
    activeRequestId,
    agentAttachments,
    chatHistory,
    isProcessing,
    liveTask.attachments,
    liveTask.content,
    liveTask.error,
    liveTask.events,
    optimisticOutgoing,
  ]);

  // Drop the optimistic outgoing once the persisted history echoes it back.
  useEffect(() => {
    if (!optimisticOutgoing) return;
    const persisted = chatHistory ?? [];
    const optAt = Date.parse(optimisticOutgoing.sentAt);
    const matched = persisted.some(
      (m) =>
        m.role === 'user' &&
        m.content === optimisticOutgoing.content &&
        Math.abs(Date.parse(m.sentAt) - optAt) < 5 * 60_000,
    );
    if (matched) setOptimisticOutgoing(null);
  }, [chatHistory, optimisticOutgoing]);

  // Capture agent-emitted inline attachments onto the persisted assistant
  // message that just landed. Heuristic: while liveTask still holds the
  // streamed attachments, find the most recent persisted assistant message
  // whose id we have not already claimed and bind the attachments to it.
  // This lets the FileView / TerminalExcerpt cards survive after the live
  // streaming row collapses into the persisted thread.
  useEffect(() => {
    if (!liveTask.attachments || liveTask.attachments.length === 0) return;
    const persisted = chatHistory ?? [];
    for (let i = persisted.length - 1; i >= 0; i--) {
      const m = persisted[i];
      if (m.role !== 'assistant' || !m.id) continue;
      if (agentAttachments[m.id]) break; // already claimed; don't double-bind
      const attachments = liveTask.attachments;
      setAgentAttachments((prev) =>
        prev[m.id!] ? prev : { ...prev, [m.id!]: attachments },
      );
      break;
    }
  }, [chatHistory, liveTask.attachments, agentAttachments]);

  // Clear captured attachments when the active chat session changes — they
  // are keyed by message ids that only make sense within one session.
  useEffect(() => {
    setAgentAttachments({});
    setOptimisticOutgoing(null);
  }, [chatId]);

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

  const chatMainFill = (
    <Fill className="bg-surface-50 dark:bg-dark-surface-100 flex flex-col">
            {chatId ? (
              <>
                <ChatToolbar
                  chainOptions={chainOptions}
                  selectedChainId={selectedChainId}
                  onChainChange={setSelectedChainId}
                  chainsLoading={chainsLoading}
                  executorChainPreview={executorChainPreview ?? null}
                  onTokenLimitSave={limit => updateChain({ token_limit: limit })}
                  modeOptions={modeOptions}
                  selectedMode={selectedMode}
                  onModeChange={setSelectedMode}
                  isProcessing={isProcessing}
                  policyNames={policyNames}
                  activePolicyName={activePolicyName}
                  onPolicyChange={name => setActivePolicy.mutate(name)}
                  policyChangePending={setActivePolicy.isPending}
                  policyChangeError={setActivePolicy.isError ? (setActivePolicy.error?.message ?? t('chat.hitl_policy_error', 'Failed to set policy')) : null}
                  statsLabel={t('chat.stats_compact', { messages: chatHistory?.length ?? 0, state: latestState.length })}
                  workspacePanelOpen={workspacePanelOpen}
                  onWorkspaceToggle={() => persistWorkspacePanelOpen(!workspacePanelOpen)}
                  onOpenMobileWorkspace={() => setMobileWorkspaceOpen(true)}
                  onEditChain={() => navigate(`/chains?path=${encodeURIComponent(selectedChainId.trim())}`)}
                  isLg={isLg}
                />

                {selectedMode === 'build' && (
                  <BuildModeStrip
                    selectedChainId={selectedChainId}
                    executorChainPreview={executorChainPreview ?? null}
                    isProcessing={isProcessing}
                    onRun={() => submitOutgoingMessage('', selectedChainId, 'build')}
                  />
                )}

                <Fill className="flex flex-col">
                  {httpDispatched && sseConnection === 'error' && (
                    <InlineNotice variant="warning">{t('chat.sse_stream_lost')}</InlineNotice>
                  )}
                  {operationError && (
                    <InlineNotice
                      variant="error"
                      onDismiss={() => {
                        setOperationError(null);
                        lastFailedSendRef.current = null;
                      }}
                    >
                      <span>{operationError}</span>
                      {lastFailedSendRef.current && (
                        <Button
                          variant="ghost"
                          size="sm"
                          className="ml-2 h-5 text-xs"
                          type="button"
                          onClick={() => {
                            const failed = lastFailedSendRef.current;
                            if (!failed) return;
                            setOperationError(null);
                            lastFailedSendRef.current = null;
                            submitOutgoingMessage(failed.text, failed.chainId, failed.mode);
                          }}
                        >
                          {t('plan.retry', 'Retry')}
                        </Button>
                      )}
                    </InlineNotice>
                  )}

                  <div className="min-h-0 flex-1">
                    {chatHistory && Array.isArray(chatHistory) && (
                      <ChatInterface
                        threadItems={threadItems}
                        compiledPlanEmbedContent={compiledPlanEmbedContent}
                        workbenchTab={workbenchTab}
                        onWorkbenchTabChange={persistWorkbenchTab}
                        showWorkbenchTabs={!!selectedChainId.trim()}
                        chainPanel={chainPanel}
                        showPlanTab={selectedMode === 'plan'}
                        isLoading={historyLoading}
                        error={error}
                        isProcessing={isProcessing}
                        processingBarLabel={processingBarLabel}
                        embedStreamThinkingInThread
                        liveThinking={liveTask.thinking}
                        canStop={isProcessing}
                        onStop={handleStop}
                        streamScrollSignature={streamScrollSignature}
                        approvalContent={liveTask.pendingApproval ? (
                          <ApprovalCard
                            approval={liveTask.pendingApproval}
                            onRespond={async (approved) => {
                              if (!liveTask.pendingApproval) return;
                              try {
                                await api.respondToApproval(liveTask.pendingApproval.approvalId, approved);
                              } catch {
                                // Backend will surface the outcome via the SSE stream.
                              }
                            }}
                          />
                        ) : undefined}
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
  );

  return (
    <Page bodyScroll="hidden" className="h-full">
      <Fill className="flex min-h-0">
        <div className="flex min-h-0 min-w-0 flex-1 flex-row">
          {chatId && workspacePanelOpen && isLg ? (
            <ResizablePanelGroup className="flex min-h-0 min-w-0 flex-1 flex-row">
              <ResizablePanel
                minSize={280}
                className="flex min-h-0 min-w-0 flex-col">
                {chatMainFill}
              </ResizablePanel>
              <ResizablePanelHandle onResizeEnd={onWorkspaceSplitResizeEnd} />
              <ResizablePanel
                ref={workspacePanelRef}
                defaultSize={workspaceSplitInitialPx != null ? `${workspaceSplitInitialPx}px` : 'min(420px,38vw)'}
                minSize={260}
                maxSize={900}
                className="border-border bg-surface-50 dark:bg-dark-surface-100 flex min-h-0 min-w-0 flex-col border-l">
                <WorkspaceSplitPanel ref={workspaceRef} className="min-h-0 flex-1 border-0" />
              </ResizablePanel>
            </ResizablePanelGroup>
          ) : (
            <>
              <div className="flex min-h-0 min-w-0 flex-1 flex-col">{chatMainFill}</div>
              {chatId && workspacePanelOpen && !isLg ? (
                <div
                  className={cn(
                    'border-border bg-surface-50 dark:bg-dark-surface-100 flex min-h-0 w-full shrink-0 flex-col border-l',
                    mobileWorkspaceOpen ? 'fixed inset-0 z-50 flex flex-col' : 'hidden',
                  )}>
                  {mobileWorkspaceOpen ? (
                    <div className="flex items-center justify-end gap-2 border-b px-2 py-1.5">
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
            </>
          )}
        </div>

        <ChatRunLog
          open={statePanelOpen}
          onToggle={toggleStatePanel}
          isProcessing={isProcessing}
          events={liveTask.events}
          state={latestState}
        />
      </Fill>
    </Page>
  );
}
