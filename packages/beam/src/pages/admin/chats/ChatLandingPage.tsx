import { InlineNotice, P, Page, Panel, Section, Select, Span, Spinner } from '@contenox/ui';
import { useMemo, useState, useEffect, FormEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { ArtifactRegistryProvider } from '../../../lib/artifacts';
import { useListFiles } from '../../../hooks/useFiles';
import { isChainLikeVfsPath } from '../../../lib/chainPaths';
import { useCreateChat } from '../../../hooks/useChats';
import { useSetupStatus } from '../../../hooks/useSetupStatus';
import { SlashCommandRegistryProvider } from '../../../lib/slashCommands';
import { ChatSession } from '../../../lib/types';
import { MessageInputForm } from './components/MessageInputForm';

/**
 * Same provider boundary as [ChatPage]: MessageInputForm uses useSlashCommandRegistry
 * and useArtifactRegistry and must render inside both providers.
 */
export default function ChatLandingPage() {
  return (
    <ArtifactRegistryProvider>
      <SlashCommandRegistryProvider>
        <ChatLandingPageImpl />
      </SlashCommandRegistryProvider>
    </ArtifactRegistryProvider>
  );
}

function ChatLandingPageImpl() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [message, setMessage] = useState('');
  const [selectedChainId, setSelectedChainId] = useState('');
  const createChat = useCreateChat();

  const { data: setupStatus } = useSetupStatus(true);
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
          {setupStatus && !setupStatus.defaultModel ? (
            <InlineNotice variant="warning">
              {t('chat.landing_no_model', 'No default model set. Run contenox init to configure.')}
            </InlineNotice>
          ) : setupStatus?.defaultModel ? (
            <Span variant="muted" className="block text-xs">
              {[setupStatus.defaultModel, setupStatus.defaultProvider].filter(Boolean).join(' · ')}
            </Span>
          ) : null}
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
          <Panel variant="surface" className="m-0">
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

