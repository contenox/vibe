import { InsetPanel, P, Span } from '@contenox/ui';
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
 * Compiled plan preview shown in the Plan tab.
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

  if (activePlanLoading && !hasActivePlan) {
    return (
      <InsetPanel tone="muted" className="flex min-h-[200px] items-center justify-center">
        <Span variant="muted" className="text-sm">
          {t('chat.build_graph_loading')}
        </Span>
      </InsetPanel>
    );
  }

  if (!hasActivePlan) {
    return (
      <P className="text-text-secondary dark:text-dark-text-muted p-3 text-sm">
        {t('chat.build_compiled_no_plan')}
      </P>
    );
  }

  if (!hasSteps) {
    return (
      <P className="text-text-secondary dark:text-dark-text-muted p-3 text-sm">
        {t('chat.build_compiled_no_steps')}
      </P>
    );
  }

  return (
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
}
