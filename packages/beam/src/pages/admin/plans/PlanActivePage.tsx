import { Button, GridLayout, P, Panel, Section } from '@contenox/ui';
import { ArrowLeft } from 'lucide-react';
import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { ErrorState } from '../../../components/LoadingState';
import { Page } from '../../../components/Page';
import { useListFiles } from '../../../hooks/useFiles';
import { useActivePlan } from '../../../hooks/usePlans';
import { isChainLikeVfsPath } from '../../../lib/chainPaths';
import ActivePlanSection from './components/ActivePlanSection';

/**
 * Workspace for the active plan (run next, replan, steps).
 */
export default function PlanActivePage() {
  const { t } = useTranslation();
  const { data: files = [], isLoading: filesLoading, error: filesError, refetch: refetchFiles } =
    useListFiles();
  const {
    data: activePlan,
    isLoading: activeLoading,
    error: activeError,
    refetch: refetchActive,
  } = useActivePlan();

  const chainPaths = useMemo(
    () => files.filter(f => isChainLikeVfsPath(f.path)).map(f => f.path),
    [files],
  );

  const handleRetry = () => {
    void refetchFiles();
    void refetchActive();
  };

  if (filesError) {
    return (
      <Page bodyScroll="auto">
        <ErrorState error={filesError as Error} onRetry={handleRetry} title={t('plans.page_error')} />
      </Page>
    );
  }

  return (
    <Page bodyScroll="auto">
      <GridLayout variant="body" className="gap-8 pb-8">
        <Section>
          <Link to="/plans">
            <Button variant="ghost" size="sm" type="button" className="mb-4">
              <ArrowLeft className="mr-2 h-4 w-4" />
              {t('plans.back_to_list')}
            </Button>
          </Link>
          <h1 className="text-2xl font-semibold">{t('plans.workspace_page_title')}</h1>
          <P variant="muted" className="mt-2 text-sm">
            {t('plans.workspace_page_description')}
          </P>
        </Section>

        {activeError && activeError instanceof Error && (
          <Panel variant="error">
            {t('plans.active_error_banner')}: {activeError.message}
          </Panel>
        )}

        <ActivePlanSection
          active={activePlan}
          isLoading={activeLoading || filesLoading}
          chainPaths={chainPaths}
          chainsLoading={filesLoading}
        />
      </GridLayout>
    </Page>
  );
}
