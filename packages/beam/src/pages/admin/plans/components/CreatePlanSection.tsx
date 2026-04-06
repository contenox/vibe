import { Button, Form, FormField, P, Panel, Section, Select, Textarea } from '@contenox/ui';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { useCreatePlan } from '../../../../hooks/usePlans';

type Props = {
  chainPaths: string[];
  chainsLoading: boolean;
  /** After a successful create, open the active-plan workspace. */
  navigateToWorkspaceOnSuccess?: boolean;
};

export default function CreatePlanSection({
  chainPaths,
  chainsLoading,
  navigateToWorkspaceOnSuccess = false,
}: Props) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [goal, setGoal] = useState('');
  const [plannerChainId, setPlannerChainId] = useState('');
  const createMutation = useCreatePlan();

  const chainOptions = [
    { value: '', label: t('plans.select_planner_chain') },
    ...chainPaths.map(p => ({ value: p, label: p })),
  ];

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const g = goal.trim();
    if (!g || !plannerChainId) return;
    createMutation.mutate(
      { goal: g, planner_chain_id: plannerChainId },
      {
        onSuccess: () => {
          setGoal('');
          if (navigateToWorkspaceOnSuccess) {
            navigate('/plans/active');
          }
        },
      },
    );
  };

  return (
    <Section>
      <h2 className="text-lg font-semibold">{t('plans.create_title')}</h2>
      <P variant="muted" className="mb-4 text-sm">
        {t('plans.chain_id_hint')}
      </P>
      <Form
        onSubmit={handleSubmit}
        title={t('plans.create_form_title')}
        actions={
          <Button
            type="submit"
            variant="primary"
            disabled={
              !goal.trim() || !plannerChainId || createMutation.isPending || chainsLoading
            }>
            {t('plans.create_submit')}
          </Button>
        }>
        <FormField label={t('plans.goal_label')} required>
          <Textarea
            value={goal}
            onChange={e => setGoal(e.target.value)}
            placeholder={t('plans.goal_placeholder')}
            rows={4}
          />
        </FormField>
        <FormField label={t('plans.planner_chain_label')} required>
          <Select
            options={chainOptions}
            value={plannerChainId}
            onChange={e => setPlannerChainId(e.target.value)}
            disabled={chainsLoading || chainPaths.length === 0}
            className="max-w-xl"
          />
        </FormField>
      </Form>
      {createMutation.isError && (
        <Panel variant="error" className="mt-4">
          {createMutation.error?.message ?? t('errors.generic_fetch')}
        </Panel>
      )}
      {createMutation.isSuccess && createMutation.data && (
        <Panel variant="raised" className="mt-4">
          <pre className="text-muted-foreground max-h-48 overflow-auto text-xs whitespace-pre-wrap">
            {createMutation.data.markdown}
          </pre>
        </Panel>
      )}
    </Section>
  );
}
