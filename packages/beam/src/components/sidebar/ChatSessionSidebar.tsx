import { Button, Span, Spinner } from '@contenox/ui';
import { t } from 'i18next';
import { MessageSquarePlus } from 'lucide-react';
import { Link, useMatch, useNavigate } from 'react-router-dom';
import { useChats, useCreateChat } from '../../hooks/useChats';
import { ChatSession } from '../../lib/types';
import { cn } from '../../lib/utils';

export function ChatSessionSidebar({ setIsOpen }: { setIsOpen: (open: boolean) => void }) {
  const navigate = useNavigate();
  const chatMatch = useMatch('/chat/:chatId');
  const activeChatId = chatMatch?.params.chatId;
  const createChatMutation = useCreateChat();
  const { data: chats, isLoading, error } = useChats();

  const handleNewChat = () => {
    createChatMutation.mutate(
      {},
      {
        onSuccess: (data: Partial<ChatSession>) => {
          if (data?.id) {
            navigate(`/chat/${data.id}`);
            setIsOpen(false);
          }
        },
      },
    );
  };

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="border-surface-300 dark:border-dark-surface-700 shrink-0 border-b p-3">
        <Button
          variant="primary"
          size="sm"
          className="w-full gap-2"
          disabled={createChatMutation.isPending}
          onClick={handleNewChat}>
          {createChatMutation.isPending ? (
            <Spinner size="sm" />
          ) : (
            <MessageSquarePlus className="h-4 w-4 shrink-0" aria-hidden />
          )}
          <Span>{t('chat.start_new_chat')}</Span>
        </Button>
      </div>
      <nav
        className="min-h-0 flex-1 space-y-1 overflow-y-auto p-3"
        aria-label={t('chat.personal_chat_list_title')}>
        {isLoading ? (
          <div className="flex items-center justify-center gap-2 py-8">
            <Spinner size="md" />
            <Span className="text-text-muted text-sm">{t('chat.loading_chats')}</Span>
          </div>
        ) : error ? (
          <Span className="text-error text-sm">{error.message || t('chat.list_error')}</Span>
        ) : !chats?.length ? (
          <Span className="text-text-muted text-sm">{t('chat.sidebar_empty_hint')}</Span>
        ) : (
          chats.map(chat => {
            const isActive = activeChatId === chat.id;
            return (
              <Link
                key={chat.id}
                to={`/chat/${chat.id}`}
                onClick={() => setIsOpen(false)}
                className={cn(
                  'block rounded-lg py-2 pr-2 pl-4 text-left transition-colors',
                  isActive
                    ? 'bg-primary-100 dark:bg-dark-primary-900 text-text font-medium'
                    : 'text-text hover:bg-surface-100 dark:hover:bg-dark-surface-100',
                )}>
                <Span className="line-clamp-1 block text-sm">{chat.model || chat.id}</Span>
                {chat.lastMessage?.content ? (
                  <Span className="text-text-muted line-clamp-2 block text-xs">
                    {chat.lastMessage.content.length > 80
                      ? chat.lastMessage.content.slice(0, 77) + '…'
                      : chat.lastMessage.content}
                  </Span>
                ) : null}
              </Link>
            );
          })
        )}
      </nav>
    </div>
  );
}
