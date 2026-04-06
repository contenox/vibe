import { Button, Card, FormField, H3, Input, Select, Span, Tooltip } from '@contenox/ui';
import { HelpCircle, Plus, Trash2 } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { FormTransition, OperatorTerm } from '../../../../../../lib/types';

type Props = {
  transition: FormTransition;
  onChange: (next: FormTransition) => void;
  availableVariables?: string[];
};

const OPERATOR_OPTIONS: { value: OperatorTerm; label: string; description?: string }[] = [
  {
    value: 'default',
    label: 'default',
    description: 'Always take this branch if no other conditions match',
  },
  {
    value: 'equals',
    label: 'equals',
    description: 'Exact match with the condition value',
  },
  {
    value: 'contains',
    label: 'contains',
    description: 'Output contains the specified text',
  },
  {
    value: 'starts_with',
    label: 'starts_with',
    description: 'Output begins with the specified text',
  },
  {
    value: 'ends_with',
    label: 'ends_with',
    description: 'Output ends with the specified text',
  },
  {
    value: 'gt',
    label: 'gt',
    description: 'Output is greater than the specified number',
  },
  {
    value: 'lt',
    label: 'lt',
    description: 'Output is less than the specified number',
  },
  {
    value: 'in_range',
    label: 'in_range',
    description: 'Output falls within the specified range (e.g., "5-10")',
  },
];

export default function TransitionEditor({
  transition,
  onChange,
  availableVariables = ['input'],
}: Props) {
  const { t } = useTranslation();

  const gotoOptions = [
    ...availableVariables.map(v => ({ value: v, label: v })),
    { value: 'end', label: 'end' },
  ];

  const setFailure = (on_failure?: string) =>
    onChange({
      ...transition,
      on_failure,
    });

  const updateBranch = (index: number, updates: Partial<FormTransition['branches'][number]>) => {
    const next = [...(transition.branches || [])];
    next[index] = { ...next[index], ...updates };
    onChange({ ...transition, branches: next });
  };

  const addBranch = () => {
    onChange({
      ...transition,
      branches: [...(transition.branches || []), { when: '', operator: 'default', goto: 'end' }],
    });
  };

  const removeBranch = (index: number) => {
    const next = [...(transition.branches || [])];
    next.splice(index, 1);
    onChange({ ...transition, branches: next });
  };

  const getOperatorDescription = (operator: string) => {
    const op = OPERATOR_OPTIONS.find(o => o.value === operator);
    return op?.description || '';
  };

  return (
    <div className="space-y-6">
      <div>
        <div className="mb-2 flex items-center gap-2">
          <H3>{t('workflow.on_failure')}</H3>
          <Tooltip content="Task to execute when this task fails (after retries)">
            <HelpCircle className="text-muted-foreground h-4 w-4 cursor-help" />
          </Tooltip>
        </div>
        <Select
          value={transition.on_failure || ''}
          onChange={e => setFailure(e.target.value || undefined)}
          options={[{ value: '', label: t('common.none') }, ...gotoOptions]}
          placeholder={t('common.none')}
        />
      </div>

      <Card className="space-y-4 p-4">
        {(transition.branches || []).map((b, i) => (
          <Card key={i} variant="surface" className="p-4">
            <div className="grid grid-cols-1 gap-4 md:grid-cols-3">
              <FormField label={t('workflow.condition')}>
                <Input
                  value={b.when || ''}
                  onChange={e => updateBranch(i, { when: e.target.value })}
                  placeholder={t('workflow.condition_placeholder')}
                  className={b.operator !== 'default' ? 'border-blue-300' : ''}
                />
                {b.operator !== 'default' && (
                  <p className="text-muted-foreground mt-1 text-xs">
                    Will be compared using {b.operator} operator
                  </p>
                )}
              </FormField>

              <FormField
                label={
                  <div className="flex items-center gap-2">
                    {t('workflow.operator')}
                    <Tooltip content={getOperatorDescription(b.operator || 'default')}>
                      <HelpCircle className="text-muted-foreground h-4 w-4 cursor-help" />
                    </Tooltip>
                  </div>
                }>
                <Select
                  value={(b.operator as string) || 'default'}
                  onChange={e =>
                    updateBranch(i, { operator: (e.target.value as OperatorTerm) || 'default' })
                  }
                  options={OPERATOR_OPTIONS.map(op => ({
                    value: op.value,
                    label: op.label,
                  }))}
                  placeholder="operator"
                />
              </FormField>

              <FormField
                label={
                  <div className="flex items-center gap-2">
                    {t('workflow.goto_task')}
                    <Tooltip content="Task ID or 'end' to terminate the workflow">
                      <HelpCircle className="text-muted-foreground h-4 w-4 cursor-help" />
                    </Tooltip>
                  </div>
                }>
                <Select
                  value={b.goto || 'end'}
                  onChange={e => updateBranch(i, { goto: e.target.value })}
                  options={gotoOptions}
                  placeholder="goto"
                />
              </FormField>
            </div>

            {/* Compose Configuration Section */}
            <div className="mt-4 border-t pt-4">
              <div className="mb-3 flex items-center gap-2">
                <Span className="text-sm font-medium">Compose Configuration</Span>
                <Tooltip content="Configure how the output should be transformed before transitioning">
                  <HelpCircle className="text-muted-foreground h-4 w-4 cursor-help" />
                </Tooltip>
              </div>

              <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
                <FormField label="With Variable">
                  <Select
                    value={b.compose?.with_var || ''}
                    onChange={e => {
                      const newCompose = b.compose || { with_var: '', strategy: 'override' };
                      updateBranch(i, {
                        compose: {
                          ...newCompose,
                          with_var: e.target.value,
                        },
                      });
                    }}
                    options={[
                      { value: '', label: 'Select variable...' },
                      ...availableVariables
                        .filter(v => v !== b.goto) // Filter out the current task
                        .map(v => ({ value: v, label: v })),
                    ]}
                    placeholder="Select variable to compose with"
                  />
                </FormField>

                <FormField label="Strategy">
                  <Select
                    value={b.compose?.strategy || 'override'}
                    onChange={e => {
                      const newCompose = b.compose || { with_var: '', strategy: 'override' };
                      updateBranch(i, {
                        compose: {
                          ...newCompose,
                          strategy: e.target.value,
                        },
                      });
                    }}
                    options={[
                      { value: 'override', label: 'Override' },
                      { value: 'merge_chat_histories', label: 'Merge Chat Histories' },
                      { value: 'append_string_to_chat_history', label: 'Append to Chat History' },
                      { value: 'concat_strings', label: 'Concatenate Strings' },
                      { value: 'sum_numbers', label: 'Sum Numbers' },
                    ]}
                    placeholder="Select strategy"
                  />
                </FormField>
              </div>

              {b.compose?.strategy && b.compose?.with_var && (
                <div className="mt-3 rounded-md bg-blue-50 p-3">
                  <p className="text-sm text-blue-800">
                    {b.compose.strategy === 'override' &&
                      `Output will override ${b.compose.with_var}`}
                    {b.compose.strategy === 'merge_chat_histories' &&
                      `Chat histories will be merged`}
                    {b.compose.strategy === 'append_string_to_chat_history' &&
                      `Output will be appended to chat history`}
                  </p>
                </div>
              )}
            </div>

            <div className="mt-4 flex justify-end">
              <Button
                size="sm"
                variant="danger"
                onClick={() => removeBranch(i)}>
                <Trash2 className="mr-2 h-4 w-4" />
                {t('common.delete_branch')}
              </Button>
            </div>
          </Card>
        ))}

        <Button onClick={addBranch} variant="secondary" className="w-full">
          <Plus className="mr-2 h-4 w-4" />
          {t('workflow.add_branch')}
        </Button>

        <div className="mt-2 rounded-md bg-amber-50 p-3">
          <p className="text-xs text-amber-800">
            <strong>Note:</strong> Compose operations happen BEFORE the transition. For branches
            going to "end", compose configurations prepare the final workflow output (e.g.,
            appending messages to chat history).
          </p>
        </div>
      </Card>
    </div>
  );
}
