import {
  Button,
  Checkbox,
  Collapsible,
  FormField,
  P,
  Panel,
  Section,
  Select,
  Span,
  Table,
  TableCell,
  TableRow,
  Tooltip,
} from '@contenox/ui';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  usePlanNext,
  usePlanReplan,
  useRetryPlanStep,
  useSkipPlanStep,
} from '../../../../hooks/usePlans';
import type { ActivePlanResponse } from '../../../../lib/types';

type Props = {
  active: ActivePlanResponse | null | undefined;
  isLoading: boolean;
  chainPaths: string[];
  chainsLoading: boolean;
};

export default function ActivePlanSection({
  active,
  isLoading,
  chainPaths,
  chainsLoading,
}: Props) {
  const { t } = useTranslation();
  const [executorChainId, setExecutorChainId] = useState('');
  const [replanChainId, setReplanChainId] = useState('');
  const [withShell, setWithShell] = useState(false);
  const [withAuto, setWithAuto] = useState(false);
  const [lastMarkdown, setLastMarkdown] = useState<string | null>(null);

  const nextMutation = usePlanNext();
  const replanMutation = usePlanReplan();
  const retryMutation = useRetryPlanStep();
  const skipMutation = useSkipPlanStep();

  const emptyOption = { value: '', label: t('plans.select_executor_chain') };
  const chainOptions = [emptyOption, ...chainPaths.map(p => ({ value: p, label: p }))];
  const replanOptions = [emptyOption, ...chainPaths.map(p => ({ value: p, label: p }))];

  const handleNext = (e: React.FormEvent) => {
    e.preventDefault();
    if (!executorChainId) return;
    setLastMarkdown(null);
    nextMutation.mutate(
      { executor_chain_id: executorChainId, with_shell: withShell, with_auto: withAuto },
      {
        onSuccess: data => {
          setLastMarkdown(data.markdown);
        },
      },
    );
  };

  const handleReplan = (e: React.FormEvent) => {
    e.preventDefault();
    if (!replanChainId) return;
    setLastMarkdown(null);
    replanMutation.mutate(
      { planner_chain_id: replanChainId },
      {
        onSuccess: data => {
          setLastMarkdown(data.markdown);
        },
      },
    );
  };

  if (isLoading) {
    return (
      <Section>
        <h2 className="text-lg font-semibold">{t('plans.active_title')}</h2>
        <P variant="muted">{t('plans.active_loading')}</P>
      </Section>
    );
  }

  if (active == null) {
    return (
      <Section>
        <h2 className="text-lg font-semibold">{t('plans.active_title')}</h2>
        <Panel variant="bordered" className="mt-4 py-8 text-center">
          <Span variant="muted">{t('plans.no_active')}</Span>
        </Panel>
      </Section>
    );
  }

  const { plan, steps } = active;
  const busy =
    nextMutation.isPending ||
    replanMutation.isPending ||
    retryMutation.isPending ||
    skipMutation.isPending;

  return (
    <Section className="space-y-6">
      <div>
        <h2 className="text-lg font-semibold">{t('plans.active_title')}</h2>
        <P variant="muted" className="mt-1 text-sm">
          <Span className="font-medium text-foreground">{plan.name}</Span>
          {' — '}
          {plan.goal}
        </P>
      </div>

      <form onSubmit={handleNext} className="space-y-4 rounded-lg border p-4">
        <Span className="text-sm font-medium">{t('plans.run_next_step')}</Span>
        <FormField label={t('plans.executor_chain_label')}>
          <Select
            options={chainOptions}
            value={executorChainId}
            onChange={e => setExecutorChainId(e.target.value)}
            disabled={chainsLoading || chainPaths.length === 0}
            className="max-w-xl"
          />
        </FormField>
        <div className="flex flex-wrap gap-6">
          <Checkbox
            checked={withShell}
            onChange={e => setWithShell(e.target.checked)}
            label={t('plans.with_shell')}
          />
          <Checkbox
            checked={withAuto}
            onChange={e => setWithAuto(e.target.checked)}
            label={t('plans.with_auto')}
          />
        </div>
        <Button type="submit" variant="primary" disabled={!executorChainId || busy}>
          {t('plans.next_submit')}
        </Button>
        {nextMutation.isError && (
          <Panel variant="error">{nextMutation.error?.message}</Panel>
        )}
      </form>

      <form onSubmit={handleReplan} className="space-y-4 rounded-lg border p-4">
        <Span className="text-sm font-medium">{t('plans.replan_title')}</Span>
        <FormField label={t('plans.planner_chain_label')}>
          <Select
            options={replanOptions}
            value={replanChainId}
            onChange={e => setReplanChainId(e.target.value)}
            disabled={chainsLoading || chainPaths.length === 0}
            className="max-w-xl"
          />
        </FormField>
        <Button type="submit" variant="outline" disabled={!replanChainId || busy}>
          {t('plans.replan_submit')}
        </Button>
        {replanMutation.isError && (
          <Panel variant="error">{replanMutation.error?.message}</Panel>
        )}
      </form>

      {lastMarkdown && (
        <Collapsible title={t('plans.markdown_output')} defaultExpanded>
          <pre className="text-muted-foreground max-h-64 overflow-auto text-xs whitespace-pre-wrap">
            {lastMarkdown}
          </pre>
        </Collapsible>
      )}

      <div>
        <h3 className="mb-2 text-sm font-medium">{t('plans.steps_title')}</h3>
        <Table
          columns={[
            t('plans.col_ordinal'),
            t('plans.col_description'),
            t('plans.col_step_status'),
            t('plans.col_result'),
            t('plans.col_actions'),
          ]}>
          {steps.map(step => (
            <TableRow key={step.id}>
              <TableCell>{step.ordinal}</TableCell>
              <TableCell className="max-w-[280px]">
                <Tooltip content={step.description}>
                  <Span className="line-clamp-2">{step.description}</Span>
                </Tooltip>
              </TableCell>
              <TableCell>
                <Span className="bg-secondary inline-flex rounded-full px-2 py-0.5 text-xs">
                  {step.status}
                </Span>
              </TableCell>
              <TableCell className="max-w-[200px]">
                <Tooltip content={step.execution_result || '—'}>
                  <Span className="line-clamp-2 text-xs">
                    {step.execution_result || '—'}
                  </Span>
                </Tooltip>
              </TableCell>
              <TableCell>
                <div className="flex flex-wrap gap-2">
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    disabled={busy}
                    onClick={() =>
                      retryMutation.mutate(step.ordinal, {
                        onSuccess: d => setLastMarkdown(d.markdown),
                      })
                    }>
                    {t('plans.retry_step')}
                  </Button>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    disabled={busy}
                    onClick={() =>
                      skipMutation.mutate(step.ordinal, {
                        onSuccess: d => setLastMarkdown(d.markdown),
                      })
                    }>
                    {t('plans.skip_step')}
                  </Button>
                </div>
              </TableCell>
            </TableRow>
          ))}
        </Table>
        {(retryMutation.isError || skipMutation.isError) && (
          <Panel variant="error" className="mt-2">
            {retryMutation.error?.message || skipMutation.error?.message}
          </Panel>
        )}
      </div>
    </Section>
  );
}
