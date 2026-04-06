import { P, Panel, Section, Select, Span, Spinner } from '@contenox/ui';
import { useMemo, useState, useEffect, FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { Page } from '../../../components/Page';
import { useListFiles } from '../../../hooks/useFiles';
import { isChainLikeVfsPath } from '../../../lib/chainPaths';
import { useCreateChat } from '../../../hooks/useChats';
import { ChatSession } from '../../../lib/types';
import { MessageInputForm } from './components/MessageInputForm';

export default function ChatLandingPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [message, setMessage] = useState('');
  const [selectedChainId, setSelectedChainId] = useState('');
  const createChat = useCreateChat();

  const { data: files = [], isLoading: chainsLoading } = useListFiles();
  const chainPaths = useMemo(
    () => files.filter(f => isChainLikeVfsPath(f.path)).map(f => f.path),
    [files],
  );

  useEffect(() => {
    if (selectedChainId) return;
    if (chainPaths.length === 1) setSelectedChainId(chainPaths[0]);
  }, [chainPaths, selectedChainId]);

  const chainOptions = useMemo(
    () => [
      { value: '', label: t('chat.no_chain') },
      ...chainPaths.map(p => ({ value: p, label: p })),
    ],
    [chainPaths, t],
  );

  const handleSubmit = (e: FormEvent) => {
    e.preventDefault();
    const trimmed = message.trim();
    if (!trimmed || !selectedChainId) return;
    createChat.mutate(
      {},
      {
        onSuccess: (data: Partial<ChatSession>) => {
          if (data?.id) {
            navigate(`/chat/${data.id}`, {
              replace: true,
              state: {
                beamInitialMessage: trimmed,
                beamInitialChainId: selectedChainId,
              },
            });
          }
        },
      },
    );
  };

  const canSend = !!selectedChainId && !!message.trim() && !createChat.isPending;

  return (
    <Page bodyScroll="auto">
      <Section title={t('chat.landing_title')} description={t('chat.landing_description')}>
        <div className="mx-auto mt-6 max-w-2xl space-y-4">
          <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
            <Span variant="muted" className="shrink-0 text-sm">
              {t('chat.task_chain')}
            </Span>
            <Select
              options={chainOptions}
              value={selectedChainId}
              onChange={e => setSelectedChainId(e.target.value)}
              className="min-w-[12rem] flex-1"
              disabled={chainsLoading}
            />
            {chainsLoading && <Spinner size="sm" />}
          </div>
          <Panel className="bg-surface-50 dark:bg-dark-surface-100">
            <MessageInputForm
              value={message}
              onChange={setMessage}
              onSubmit={handleSubmit}
              isPending={createChat.isPending}
              placeholder={t('chat.landing_input_placeholder')}
              title=""
              variant="compact"
              canSubmit={canSend}
            />
          </Panel>
          {createChat.isError && (
            <P className="text-error text-sm">
              {createChat.error?.message ?? t('chat.error_create_chat')}
            </P>
          )}
        </div>
      </Section>
    </Page>
  );
}
