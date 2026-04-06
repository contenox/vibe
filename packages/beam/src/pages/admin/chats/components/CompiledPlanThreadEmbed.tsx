import { InsetPanel, P, Span, TranscriptEmbedCard } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import type { ChainDefinition } from '../../../../lib/types';
import BuildModeChainGraph from './BuildModeChainGraph';

type Props = {
  chain: ChainDefinition | null;
  caption: string | null;
  isLoading: boolean;
  error: Error | null;
  activePlanLoading: boolean;
  hasActivePlan: boolean;
  hasSteps: boolean;
};

/**
 * Client-only compiled plan preview inside the transcript (not a persisted message).
 */
export function CompiledPlanThreadEmbed({
  chain,
  caption,
  isLoading,
  error,
  activePlanLoading,
  hasActivePlan,
  hasSteps,
}: Props) {
  const { t } = useTranslation();

  const body = activePlanLoading && !hasActivePlan ? (
    <InsetPanel tone="muted" className="flex min-h-[200px] items-center justify-center">
      <Span variant="muted" className="text-sm">
        {t('chat.build_graph_loading')}
      </Span>
    </InsetPanel>
  ) : !hasActivePlan ? (
    <P className="text-text-secondary dark:text-dark-text-muted p-3 text-sm">
      {t('chat.build_compiled_no_plan')}
    </P>
  ) : !hasSteps ? (
    <P className="text-text-secondary dark:text-dark-text-muted p-3 text-sm">
      {t('chat.build_compiled_no_steps')}
    </P>
  ) : (
    <BuildModeChainGraph
      chain={
        chain ?? {
          id: 'compiled-plan-preview',
          description: '',
          tasks: [],
        }
      }
      caption={caption}
      isLoading={isLoading}
      error={error}
    />
  );

  return (
    <TranscriptEmbedCard title={t('chat.compiled_plan_embed_title')}>
      <div className="min-h-[200px] min-w-0">{body}</div>
    </TranscriptEmbedCard>
  );
}
