import {
  ChatDateSeparator,
  ChatProcessingBar,
  ChatScrollToLatest,
  ChatThread,
  ChatThreadSkeleton,
  EmptyState,
  useChatScroll,
} from '@contenox/ui';
import { t } from 'i18next';
import { ChatMessage as ApiChatMessage } from '../../../../lib/types';
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

export type ChatInterfaceProps = {
  chatHistory?: ApiChatMessage[];
  isLoading: boolean;
  error: Error | null;
  isProcessing?: boolean;
  liveThinking?: string;
  liveStatus?: string;
  canStop?: boolean;
  onStop?: () => void;
};

export const ChatInterface = ({
  chatHistory,
  isLoading,
  error,
  isProcessing = false,
  liveThinking,
  liveStatus,
  canStop = false,
  onStop,
}: ChatInterfaceProps) => {
  const { containerRef, endRef, scrollToEnd, isNearBottom } = useChatScroll({
    deps: [chatHistory],
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

  return (
    <div className="flex h-full flex-col">
      {isProcessing && (
        <>
          <ChatProcessingBar
            label={liveStatus || t('chat.thinking')}
            onStop={canStop ? onStop : undefined}
            stopLabel={t('chat.stop')}
          />
          {liveThinking && (
            <div className="border-primary-200 bg-primary-50 text-text dark:bg-dark-surface-200 dark:text-dark-text mx-4 mt-3 rounded-lg border px-3 py-2 text-sm whitespace-pre-wrap">
              {liveThinking}
            </div>
          )}
        </>
      )}

      <div className="relative min-h-0 flex-1">
        <ChatThread containerRef={containerRef} endRef={endRef} className="h-full">
          {!chatHistory?.length ? (
            <EmptyState
              title={t('chat.no_messages')}
              description={t('chat.start_conversation')}
              icon="💭"
              orientation="vertical"
              className="h-full"
            />
          ) : (
            chatHistory.map((message, index) => {
              const prevDate = index > 0 ? dateKey(chatHistory[index - 1].sentAt) : null;
              const curDate = dateKey(message.sentAt);
              const showSeparator = curDate !== prevDate;

              return (
                <div
                  key={message.id ?? `${message.sentAt}-${index}`}
                  className="animate-in fade-in-0 slide-in-from-bottom-2 duration-300"
                >
                  {showSeparator && (
                    <ChatDateSeparator
                      label={formatDateLabel(new Date(message.sentAt))}
                      className="mb-4"
                    />
                  )}
                  <ChatMessage
                    message={message}
                    isLatest={index === chatHistory.length - 1}
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
    </div>
  );
};
