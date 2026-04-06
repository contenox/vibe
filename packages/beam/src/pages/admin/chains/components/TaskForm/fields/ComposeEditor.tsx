import { Button, Input, Label, P, Panel, Select, Span } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import { BranchCompose, ComposeStrategy } from '../../../../../../lib/types';

interface ComposeEditorProps {
  taskId?: string;
  compose?: BranchCompose;
  onChange: (next?: BranchCompose) => void;
}

export default function ComposeEditor({ taskId, compose, onChange }: ComposeEditorProps) {
  const { t } = useTranslation();

  if (!compose) {
    return (
      <div className="py-4 text-center">
        <Button
          type="button"
          onClick={() => onChange({ with_var: '', strategy: 'override' })}
          className="text-primary hover:text-primary-dark text-sm">
          {t('chains.task_form.enable_compose')}
        </Button>
      </div>
    );
  }

  const strategyOptions = [
    { value: 'override', label: t('chains.task_form.compose_override') },
    { value: 'merge_chat_histories', label: t('chains.task_form.compose_merge_chat') },
    { value: 'append_string_to_chat_history', label: t('chains.task_form.compose_append_chat') },
  ];

  const handleWithVarChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    onChange({
      ...compose,
      with_var: e.target.value,
    });
  };

  const handleStrategyChange = (e: React.ChangeEvent<HTMLSelectElement>) => {
    onChange({
      ...compose,
      strategy: e.target.value as ComposeStrategy,
    });
  };

  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-4">
        <div>
          <Label className="block text-sm font-medium">
            {t('chains.task_form.compose_with_var')} <Span className="text-error">*</Span>
          </Label>
          <Input
            value={compose.with_var ?? ''}
            onChange={handleWithVarChange}
            placeholder="other_task_output"
          />
          <p className="text-text-muted mt-1 text-xs">
            {t('chains.task_form.compose_with_var_help')}
          </p>
        </div>

        <div>
          <Label className="block text-sm font-medium">
            {t('chains.task_form.compose_strategy')} <Span className="text-error">*</Span>
          </Label>
          <Select
            value={compose.strategy ?? 'override'}
            options={strategyOptions}
            onChange={handleStrategyChange}
          />
          <P className="text-text-muted mt-1 text-xs">
            {t('chains.task_form.compose_strategy_help')}
          </P>
        </div>
      </div>

      {compose.strategy ? (
        <Panel variant="surface" className="m-0 p-3">
          <P className="text-text-muted text-sm">{compose.strategy}</P>
        </Panel>
      ) : null}

      <div className="flex items-center justify-between">
        <Span className="text-text-muted text-sm">
          {t('chains.task_form.compose_output_var')}:{' '}
          <code>{taskId ? `${taskId}_composed` : 'task_id_composed'}</code>
        </Span>
        <Button
          type="button"
          onClick={() => onChange(undefined)}
          className="text-error hover:text-error-dark text-sm">
          {t('chains.task_form.disable_compose')}
        </Button>
      </div>
    </div>
  );
}
