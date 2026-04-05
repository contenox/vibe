import { Section } from '@contenox/ui';
import { t } from 'i18next';
import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useChats, useCreateChat } from '../../../../hooks/useChats';
import { ChatSession } from '../../../../lib/types';
import { ChatList } from './ChatList';
import { ChatsForm } from './ChatsForm';

export default function ChatsListPage() {
  const navigate = useNavigate();
  const [selectedModel, setSelectedModel] = useState('');

  const createChatMutation = useCreateChat();
  const { data: chats, isLoading, error } = useChats();

  const handleStartChat = (e: React.FormEvent) => {
    e.preventDefault();
    createChatMutation.mutate(
      {},
      {
        onSuccess: (data: Partial<ChatSession>) => {
          if (data?.id) navigate(`/chat/${data.id}`);
        },
      },
    );
  };

  const handleResumeChat = (chatId: string) => {
    navigate(`/chat/${chatId}`);
  };

  // Construct error message
  const errorMessage = createChatMutation.error
    ? t('chat.create_error', 'Failed to create chat: {{error}}', {
        error: createChatMutation.error.message,
      })
    : undefined;

  return (
    <Section title={t('chat.personal_chat_list_title')}>
      <ChatList
        chats={chats || []}
        isLoading={isLoading}
        error={error}
        onResumeChat={handleResumeChat}
      />
    </Section>
  );
}
