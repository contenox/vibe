import { Button, FormField, H3, P, Panel, Section, Select, Span, Tooltip } from '@contenox/ui';
import { HelpCircle } from 'lucide-react';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { BranchCompose } from '../../../../../lib/types';

interface ComposeEditorPanelProps {
  sourceTaskId: string;
  targetTaskId: string;
  composeConfig?: BranchCompose;
  onSave: (composeConfig: BranchCompose) => void;
  onCancel: () => void;
  onDelete: () => void;
  availableVariables?: string[];
}

const COMPOSE_STRATEGIES = [
  {
    value: 'override',
    label: 'Override',
    description: 'Output from the source task will completely replace the target variable',
  },
  {
    value: 'merge_chat_histories',
    label: 'Merge Chat Histories',
    description: 'Combine chat histories from both tasks, maintaining conversation context',
  },
  {
    value: 'append_string_to_chat_history',
    label: 'Append String to Chat History',
    description: 'Add the string output as a new message to the chat history',
  },
];

export const ComposeEditorPanel: React.FC<ComposeEditorPanelProps> = ({
  sourceTaskId,
  targetTaskId,
  composeConfig,
  onSave,
  onCancel,
  onDelete,
  availableVariables = ['input'],
}) => {
  const { t } = useTranslation();
  const [formData, setFormData] = useState<BranchCompose>({
    with_var: '',
    strategy: 'override',
    ...composeConfig,
  });

  useEffect(() => {
    setFormData({
      with_var: '',
      strategy: 'override',
      ...composeConfig,
    });
  }, [composeConfig]);

  const handleSave = () => {
    if (!formData.with_var) {
      alert(t('chains.compose_with_var_required'));
      return;
    }
    onSave(formData);
  };

  const strategyOptions = COMPOSE_STRATEGIES.map(strategy => ({
    value: strategy.value,
    label: strategy.label,
  }));

  const variableOptions = availableVariables
    .filter(variable => variable !== sourceTaskId) // Can't compose with self
    .map(variable => ({
      value: variable,
      label: variable,
    }));

  const getCurrentStrategy = () => {
    return COMPOSE_STRATEGIES.find(s => s.value === formData.strategy);
  };

  return (
    <Panel className="flex h-full flex-col">
      <Section
        title={t('chains.compose_editor.title')}
        description={t('chains.compose_editor.description')}
        className="shrink-0 border-b p-6">
        <div className="text-muted-foreground text-sm">
          {sourceTaskId} → {targetTaskId === 'end' ? 'workflow end' : targetTaskId}
        </div>
      </Section>

      <div className="flex-1 space-y-6 overflow-y-auto p-6">
        <FormField
          label={
            <div className="flex items-center gap-2">
              {t('chains.compose_with_variable')}
              <Tooltip content={t('chains.compose_with_variable_help')}>
                <HelpCircle className="text-muted-foreground h-4 w-4 cursor-help" />
              </Tooltip>
            </div>
          }
          required>
          <Select
            value={formData.with_var}
            onChange={e => setFormData({ ...formData, with_var: e.target.value })}
            options={variableOptions}
            placeholder={t('chains.select_variable')}
          />
        </FormField>

        <FormField
          label={
            <div className="flex items-center gap-2">
              {t('chains.compose_strategy')}
              <Tooltip content={t('chains.compose_strategy_help')}>
                <HelpCircle className="text-muted-foreground h-4 w-4 cursor-help" />
              </Tooltip>
            </div>
          }>
          <Select
            value={formData.strategy || 'override'}
            onChange={e => setFormData({ ...formData, strategy: e.target.value })}
            options={strategyOptions}
          />
        </FormField>

        {/* Strategy Description */}
        {getCurrentStrategy() && (
          <Panel variant="info" className="m-0">
            <H3 className="mb-2 flex items-center gap-2">
              <span>{getCurrentStrategy()?.label}</span>
              <Tooltip content="This strategy determines how the values are combined">
                <HelpCircle className="h-4 w-4 text-info dark:text-dark-info" />
              </Tooltip>
            </H3>
            <P className="text-sm">{getCurrentStrategy()?.description}</P>
            <Panel variant="surface" className="m-0 mt-3 p-3">
              <Span className="text-xs font-medium">
                {formData.strategy === 'override' &&
                  `${sourceTaskId} will replace ${formData.with_var}`}
                {formData.strategy === 'merge_chat_histories' &&
                  `Chat histories from ${sourceTaskId} and ${formData.with_var} will be combined`}
                {formData.strategy === 'append_string_to_chat_history' &&
                  `${sourceTaskId} will be added as a message to ${formData.with_var}'s history`}
              </Span>
            </Panel>
          </Panel>
        )}

        {/* Usage Notes */}
        <Panel variant="warning" className="m-0">
          <H3 className="mb-2">Important Notes</H3>
          <ul className="space-y-1 text-xs">
            <li>• Compose operations happen BEFORE the transition to the next task</li>
            <li>
              • The result is stored in a variable named <code>{sourceTaskId}_composed</code>
            </li>
            <li>• For transitions to "end", compose prepares the final output of the workflow</li>
          </ul>
        </Panel>
      </div>

      <div className="bg-background/50 border-t p-6">
        <div className="flex items-center justify-between">
          <div>
            {composeConfig && (
              <Button variant="danger" onClick={onDelete}>
                {t('common.delete')}
              </Button>
            )}
          </div>
          <div className="flex gap-3">
            <Button variant="secondary" onClick={onCancel} className="min-w-20">
              {t('common.cancel')}
            </Button>
            <Button variant="primary" onClick={handleSave} className="min-w-20">
              {t('common.save')}
            </Button>
          </div>
        </div>
      </div>
    </Panel>
  );
};

export default ComposeEditorPanel;
