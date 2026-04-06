import { Badge, Button, Panel, Section, Span, Table, TableCell, TableRow, Tooltip } from '@contenox/ui';
import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router-dom';
import { useActivatePlan, useDeletePlan } from '../../../../hooks/usePlans';
import type { Plan } from '../../../../lib/types';

type Props = {
  plans: Plan[] | undefined;
  /** Name of the currently active plan (for workspace link). */
  activePlanName?: string | null;
};

export default function PlanListSection({ plans, activePlanName = null }: Props) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const activateMutation = useActivatePlan();
  const deleteMutation = useDeletePlan();

  const sorted = useMemo(() => {
    if (!plans?.length) return [];
    return [...plans].sort(
      (a, b) => new Date(b.updated_at).getTime() - new Date(a.updated_at).getTime(),
    );
  }, [plans]);

  const handleActivate = (name: string) => {
    activateMutation.mutate(name, {
      onSuccess: () => navigate('/plans/active'),
    });
  };

  const handleDelete = (name: string) => {
    if (window.confirm(t('plans.delete_confirm', { name }))) {
      deleteMutation.mutate(name);
    }
  };

  return (
    <Section>
      <h2 className="text-lg font-semibold">{t('plans.list_title')}</h2>
      {!sorted.length ? (
        <Panel variant="bordered" className="mt-4 py-8 text-center">
          <Span variant="muted">{t('plans.list_empty')}</Span>
        </Panel>
      ) : (
        <Table
          className="mt-4"
          columns={[
            t('plans.col_name'),
            t('plans.col_goal'),
            t('plans.col_status'),
            t('plans.col_updated'),
            t('plans.col_actions'),
            t('plans.col_workspace'),
          ]}>
          {sorted.map(plan => (
            <TableRow key={plan.id}>
              <TableCell className="max-w-[180px] font-mono text-sm">
                <Tooltip content={plan.name}>
                  <Span className="line-clamp-2">{plan.name}</Span>
                </Tooltip>
              </TableCell>
              <TableCell className="max-w-[240px]">
                <Tooltip content={plan.goal}>
                  <Span className="line-clamp-2">{plan.goal}</Span>
                </Tooltip>
              </TableCell>
              <TableCell>
                <Badge variant="secondary" size="sm">
                  {plan.status}
                </Badge>
              </TableCell>
              <TableCell className="text-muted-foreground text-sm whitespace-nowrap">
                {new Date(plan.updated_at).toLocaleString()}
              </TableCell>
              <TableCell>
                <div className="flex flex-wrap gap-2">
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    onClick={() => handleActivate(plan.name)}
                    disabled={activateMutation.isPending || deleteMutation.isPending}>
                    {t('plans.activate')}
                  </Button>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    onClick={() => handleDelete(plan.name)}
                    disabled={activateMutation.isPending || deleteMutation.isPending}>
                    {t('common.delete')}
                  </Button>
                </div>
              </TableCell>
              <TableCell>
                {activePlanName === plan.name ? (
                  <Link to="/plans/active">
                    <Button variant="outline" size="sm" type="button">
                      {t('plans.workspace_open_short')}
                    </Button>
                  </Link>
                ) : (
                  <Span variant="muted" className="text-sm">
                    —
                  </Span>
                )}
              </TableCell>
            </TableRow>
          ))}
        </Table>
      )}
      {(activateMutation.isError || deleteMutation.isError) && (
        <Panel variant="error" className="mt-4">
          {activateMutation.error?.message ||
            deleteMutation.error?.message ||
            t('errors.generic_fetch')}
        </Panel>
      )}
    </Section>
  );
}
