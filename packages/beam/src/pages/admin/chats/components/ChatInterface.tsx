import {
  ChatDateSeparator,
  ChatProcessingBar,
  ChatScrollToLatest,
  ChatThread,
  ChatThreadSkeleton,
  EmptyState,
  InlineNotice,
  TabPanel,
  TabPanels,
  Tabs,
  useChatScroll,
} from '@contenox/ui';
import { t } from 'i18next';
import type { ReactNode } from 'react';
import { Sparkles } from 'lucide-react';
import type { ChatThreadItem } from '../chatThreadItems';
import { ChatMessage } from './ChatMessage';

function formatDateLabel(date: Date): string {
  const now = new Date();
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const target = new Date(date.getFullYear(), date.getMonth(), date.getDate());
  const diffDays = Math.round((today.getTime() - target.getTime()) / 86400000);

  if (diffDays === 0) return t('chat.date_today', 'Today');
  if (diffDays === 1) return t('chat.date_yesterday', 'Yesterday');
  return date.toLocaleDateString(undefined, { year: 'numeric', month: 'long', day: 'numeric' });
}

function dateKey(iso: string): string {
  return iso.slice(0, 10);
}

export type ChatWorkbenchTabId = 'chat' | 'chain' | 'plan';

export type ChatInterfaceProps = {
  threadItems: ChatThreadItem[];
  isLoading: boolean;
  error: Error | null;
  isProcessing?: boolean;
  processingBarLabel?: string;
  embedStreamThinkingInThread?: boolean;
  liveThinking?: string;
  canStop?: boolean;
  onStop?: () => void;
  streamScrollSignature?: string;
  liveStatus?: string;
  /** Rendered inside the Plan tab. */
  compiledPlanEmbedContent: ReactNode;
  workbenchTab?: ChatWorkbenchTabId;
  onWorkbenchTabChange?: (tab: ChatWorkbenchTabId) => void;
  showWorkbenchTabs?: boolean;
  /** Executor chain preview (Chain tab). */
  chainPanel?: ReactNode;
  /** Show the Plan tab (only when plan mode is active). */
  showPlanTab?: boolean;
  /** Rendered inline in the thread after the last message (e.g. HITL approval card). */
  approvalContent?: ReactNode;
};

export const ChatInterface = ({
  threadItems,
  isLoading,
  error,
  isProcessing = false,
  processingBarLabel,
  embedStreamThinkingInThread = false,
  liveThinking,
  liveStatus,
  canStop = false,
  onStop,
  streamScrollSignature = '',
  compiledPlanEmbedContent,
  workbenchTab = 'chat',
  onWorkbenchTabChange,
  showWorkbenchTabs = false,
  chainPanel,
  showPlanTab = false,
  approvalContent,
}: ChatInterfaceProps) => {
  const { containerRef, endRef, scrollToEnd, isNearBottom } = useChatScroll({
    deps: [threadItems, streamScrollSignature, workbenchTab],
  });

  if (isLoading) {
    return <ChatThreadSkeleton />;
  }

  if (error) {
    return (
      <EmptyState
        title={t('chat.error_loading_messages')}
        description={error.message || t('chat.error_history')}
        icon="❌"
        variant="error"
        className="h-full"
      />
    );
  }

  const barLabel =
    processingBarLabel ?? (isProcessing ? liveStatus || t('chat.thinking') : '');

  const workbenchTabs = [
    { id: 'chat' as const, label: t('chat.workbench_tab_chat') },
    { id: 'chain' as const, label: t('chat.workbench_tab_chain') },
    ...(showPlanTab
      ? [{ id: 'plan' as const, label: t('chat.workbench_tab_plan', 'Plan') }]
      : []),
  ];

  let lastMessageIndex = -1;
  for (let k = threadItems.length - 1; k >= 0; k--) {
    if (threadItems[k].kind === 'message') {
      lastMessageIndex = k;
      break;
    }
  }

  const threadBody = (
    <div className="relative min-h-0 flex-1">
      <ChatThread
        containerRef={containerRef}
        endRef={endRef}
        className="h-full"
        scrollClassName="flex-1 space-y-4 overflow-auto px-4 py-4 sm:px-5">
        {!threadItems.length ? (
          <div className="flex h-full flex-col items-center justify-center">
            <EmptyState
              title="Ready to execute"
              description="Select a toolchain from the top menu to define the agent's capabilities. Type your instruction below to begin."
              icon={<Sparkles className="w-8 h-8 opacity-80 shadow-primary/20 drop-shadow-md" />}
              iconSize="lg"
              orientation="vertical"
              className="bg-muted/10 ring-1 ring-border shadow-sm max-w-lg"
            />
          </div>
        ) : (
          threadItems.map((item, index) => {
            // Skip compiled plan embeds in the thread — they're in their own tab now
            if (item.kind === 'compiledPlanEmbed') {
              return null;
            }

            const message = item.message;
            let prevMessageSentAt: string | undefined;
            for (let j = index - 1; j >= 0; j--) {
              const it = threadItems[j];
              if (it.kind === 'message') {
                prevMessageSentAt = it.message.sentAt;
                break;
              }
            }
            const prevDate = prevMessageSentAt != null ? dateKey(prevMessageSentAt) : null;
            const curDate = dateKey(message.sentAt);
            const showSeparator = prevDate == null || curDate !== prevDate;
            const isLatest = index === lastMessageIndex;

            return (
              <div
                key={message.id ?? `${message.sentAt}-${index}`}
                className="animate-in fade-in-0 duration-150">
                {showSeparator && (
                  <ChatDateSeparator
                    label={formatDateLabel(new Date(message.sentAt))}
                    className="mb-4"
                  />
                )}
                <ChatMessage
                  message={message}
                  isLatest={isLatest}
                  streamThinking={message.streaming ? liveThinking : undefined}
                />
              </div>
            );
          })
        )}
        {approvalContent && (
          <div className="animate-in fade-in-0 duration-150 pb-2">
            {approvalContent}
          </div>
        )}
      </ChatThread>
      <ChatScrollToLatest
        visible={!isNearBottom}
        onClick={scrollToEnd}
        label={t('chat.scroll_to_latest')}
      />
    </div>
  );

  const showTabs = showWorkbenchTabs && chainPanel != null && onWorkbenchTabChange;

  return (
    <div className="flex h-full min-h-0 flex-col">
      {isProcessing && (
        <>
          <ChatProcessingBar
            label={barLabel}
            onStop={canStop ? onStop : undefined}
            stopLabel={t('chat.stop')}
          />
          {liveThinking && !embedStreamThinkingInThread && (
            <InlineNotice variant="info" className="mx-4 mt-3">
              {liveThinking}
            </InlineNotice>
          )}
        </>
      )}

      {showTabs ? (
        <div className="flex min-h-0 min-w-0 flex-1 flex-col">
          <div className="border-border shrink-0 border-b px-3 pt-2 pb-0 sm:px-4">
            <Tabs
              tabs={workbenchTabs}
              activeTab={workbenchTab}
              onTabChange={id => onWorkbenchTabChange(id as ChatWorkbenchTabId)}
            />
          </div>
          <TabPanels className="min-h-0 flex-1">
            <TabPanel tabId="chat" activeTab={workbenchTab} className="min-h-0 flex-1 flex-col">
              {threadBody}
            </TabPanel>
            <TabPanel tabId="chain" activeTab={workbenchTab} className="min-h-0 flex-1 flex-col" lazy>
              <div className="text-text dark:text-dark-text flex min-h-0 flex-1 flex-col p-3 sm:p-4">
                {chainPanel}
              </div>
            </TabPanel>
            {showPlanTab && (
              <TabPanel tabId="plan" activeTab={workbenchTab} className="min-h-0 flex-1 flex-col" lazy>
                <div className="text-text dark:text-dark-text flex min-h-0 flex-1 flex-col p-3 sm:p-4">
                  {compiledPlanEmbedContent}
                </div>
              </TabPanel>
            )}
          </TabPanels>
        </div>
      ) : (
        threadBody
      )}
    </div>
  );
};
