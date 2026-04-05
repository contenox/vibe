import { Button, GridLayout, P, Panel, Section } from '@contenox/ui';
import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { ErrorState, LoadingState } from '../../../components/LoadingState';
import { Page } from '../../../components/Page';
import { useListFiles } from '../../../hooks/useFiles';
import { useActivePlan, useCleanPlans, usePlansList } from '../../../hooks/usePlans';
import { isChainLikeVfsPath } from '../../../lib/chainPaths';
import ActivePlanSection from './components/ActivePlanSection';
import CreatePlanSection from './components/CreatePlanSection';
import PlanListSection from './components/PlanListSection';

export default function PlansPage() {
  const { t } = useTranslation();
  const { data: files = [], isLoading: filesLoading, error: filesError, refetch: refetchFiles } =
    useListFiles();
  const {
    data: plans,
    isLoading: plansLoading,
    error: plansError,
    refetch: refetchPlans,
  } = usePlansList();
  const {
    data: activePlan,
    isLoading: activeLoading,
    error: activeError,
    refetch: refetchActive,
  } = useActivePlan();

  const cleanMutation = useCleanPlans();

  const chainPaths = useMemo(
    () => files.filter(f => isChainLikeVfsPath(f.path)).map(f => f.path),
    [files],
  );

  const listError = plansError || filesError;
  const listLoading = plansLoading || filesLoading;

  const handleRetry = () => {
    void refetchPlans();
    void refetchFiles();
    void refetchActive();
  };

  const handleClean = () => {
    if (window.confirm(t('plans.clean_confirm'))) {
      cleanMutation.mutate();
    }
  };

  if (plansLoading && plans === undefined) {
    return (
      <Page bodyScroll="auto">
        <LoadingState message={t('plans.page_loading')} />
      </Page>
    );
  }

  if (listError) {
    return (
      <Page bodyScroll="auto">
        <ErrorState error={listError as Error} onRetry={handleRetry} title={t('plans.page_error')} />
      </Page>
    );
  }

  return (
    <Page bodyScroll="auto">
      <GridLayout variant="body" className="gap-8 pb-8">
        <Section>
          <h1 className="text-2xl font-semibold">{t('plans.page_title')}</h1>
          <P variant="muted" className="mt-2 max-w-3xl">
            {t('plans.page_description')}
          </P>
        </Section>

        {activeError && activeError instanceof Error && (
          <Panel variant="error">
            {t('plans.active_error_banner')}: {activeError.message}
          </Panel>
        )}

        <CreatePlanSection chainPaths={chainPaths} chainsLoading={filesLoading} />

        <PlanListSection plans={plans} />

        <ActivePlanSection
          active={activePlan}
          isLoading={activeLoading}
          chainPaths={chainPaths}
          chainsLoading={filesLoading}
        />

        <Section>
          <h2 className="text-lg font-semibold">{t('plans.cleanup_title')}</h2>
          <P variant="muted" className="mb-4 text-sm">
            {t('plans.cleanup_description')}
          </P>
          <Button
            type="button"
            variant="outline"
            onClick={handleClean}
            disabled={cleanMutation.isPending}>
            {t('plans.clean_submit')}
          </Button>
          {cleanMutation.isSuccess && (
            <P variant="muted" className="mt-2 text-sm">
              {t('plans.clean_result', { count: cleanMutation.data?.removed ?? 0 })}
            </P>
          )}
          {cleanMutation.isError && (
            <Panel variant="error" className="mt-2">
              {cleanMutation.error?.message}
            </Panel>
          )}
        </Section>
      </GridLayout>
    </Page>
  );
}
