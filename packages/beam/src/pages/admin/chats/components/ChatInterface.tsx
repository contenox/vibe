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

export type ChatWorkbenchTabId = 'chat' | 'chain';

export type ChatInterfaceProps = {
  /** When workbench tabs are shown, maps messages and optional compiled-plan embed. Otherwise pass messages-only items. */
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
  /** Renders the compiled plan embed row (only used when `threadItems` contains `compiledPlanEmbed`). */
  compiledPlanEmbedContent: ReactNode;
  /** When set with `chainPanel`, shows Chat | Chain tabs above the thread / graph. */
  workbenchTab?: ChatWorkbenchTabId;
  onWorkbenchTabChange?: (tab: ChatWorkbenchTabId) => void;
  showWorkbenchTabs?: boolean;
  /** Executor chain preview (Chain tab). Use `lazy` mounting via TabPanel. */
  chainPanel?: ReactNode;
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
          <EmptyState
            title={t('chat.no_messages')}
            description={t('chat.empty_workbench_hint')}
            icon="⌁"
            orientation="vertical"
            className="h-full"
          />
        ) : (
          threadItems.map((item, index) => {
            if (item.kind === 'compiledPlanEmbed') {
              return (
                <div
                  key={`compiled-${item.key}`}
                  className="animate-in fade-in-0 duration-150">
                  {compiledPlanEmbedContent}
                </div>
              );
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
              <div className="text-text dark:text-dark-text h-full min-h-0 overflow-auto p-3 sm:p-4">
                {chainPanel}
              </div>
            </TabPanel>
          </TabPanels>
        </div>
      ) : (
        threadBody
      )}
    </div>
  );
};
