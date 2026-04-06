import { Button, ErrorState, GridLayout, LoadingState, P, Page, Panel, Section } from '@contenox/ui';
import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { useListFiles } from '../../../hooks/useFiles';
import { useActivePlan, useCleanPlans, usePlansList } from '../../../hooks/usePlans';
import { isChainLikeVfsPath } from '../../../lib/chainPaths';
import CreatePlanSection from './components/CreatePlanSection';
import PlanListSection from './components/PlanListSection';

/**
 * Plans index: create, list, cleanup. Active plan execution lives on `/plans/active`.
 */
export default function PlansListPage() {
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
  const activePlanName = activePlan?.plan?.name ?? null;

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
          <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
            <div>
              <h1 className="text-2xl font-semibold">{t('plans.page_title')}</h1>
              <P variant="muted" className="mt-2">
                {t('plans.page_description')}
              </P>
            </div>
            <Link to="/plans/active">
              <Button variant="outline" size="sm" type="button">
                {t('plans.workspace_nav')}
              </Button>
            </Link>
          </div>
        </Section>

        {activeError && activeError instanceof Error && (
          <Panel variant="error">
            {t('plans.active_error_banner')}: {activeError.message}
          </Panel>
        )}

        {activePlan && !activeLoading && (
          <Panel variant="bordered" className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
            <div className="min-w-0">
              <P variant="muted" className="text-xs font-medium tracking-wide uppercase">
                {t('plans.active_summary_label')}
              </P>
              <P className="mt-1 truncate font-mono text-sm">{activePlan.plan.name}</P>
              <P variant="muted" className="mt-1 line-clamp-2 text-sm">
                {activePlan.plan.goal}
              </P>
            </div>
            <Link to="/plans/active">
              <Button variant="primary" size="sm" type="button">
                {t('plans.workspace_open')}
              </Button>
            </Link>
          </Panel>
        )}

        <CreatePlanSection
          chainPaths={chainPaths}
          chainsLoading={filesLoading}
          navigateToWorkspaceOnSuccess
        />

        <PlanListSection plans={plans} activePlanName={activePlanName} />

        <Section>
          <H2>{t('plans.cleanup_title')}</H2>
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
