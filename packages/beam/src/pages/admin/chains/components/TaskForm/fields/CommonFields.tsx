import { FormField, Input, Select, Textarea } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import { CHAIN_HANDLER_OPTIONS } from '../../../../../../lib/chainHandlerOptions';
import { FormTask } from '../../../../../../lib/types';

type Props = {
  task: FormTask;
  onChange: (updates: Partial<FormTask>) => void;
  /** TODO: Suggestions for things like input_var, etc. */
  availableVariables?: string[];
};

export default function CommonFields({ task, onChange, availableVariables = ['input'] }: Props) {
  const { t } = useTranslation();

  const handlerOptions = CHAIN_HANDLER_OPTIONS.map(o => ({ value: o.value, label: o.label }));
  const inputVarOptions = availableVariables.map(v => ({ value: v, label: v }));

  return (
    <>
      <FormField label={t('chains.task_id')} required>
        <Input
          value={task.id || ''}
          onChange={e => onChange({ id: e.target.value })}
          placeholder={t('chains.task_id')}
        />
      </FormField>

      <FormField label={t('workflow.task_type')} required>
        <Select
          value={task.handler || ''}
          onChange={e => onChange({ handler: e.target.value as any })}
          options={handlerOptions}
          placeholder={t('workflow.task_type')}
        />
      </FormField>

      <FormField label={t('workflow.description')}>
        <Input
          value={task.description || ''}
          onChange={e => onChange({ description: e.target.value })}
          placeholder={t('workflow.description')}
        />
      </FormField>

      <FormField label={t('chains.system_instruction')}>
        <Textarea
          value={task.system_instruction || ''}
          onChange={e => onChange({ system_instruction: e.target.value })}
          className="min-h-[100px]"
          placeholder={t('chains.system_instruction')}
        />
      </FormField>

      <FormField label={t('workflow.prompt_template')}>
        <Textarea
          value={task.prompt_template || ''}
          onChange={e => onChange({ prompt_template: e.target.value })}
          className="min-h-[140px] font-mono text-sm"
          placeholder={t('workflow.prompt_template')}
        />
      </FormField>

      <FormField label={t('workflow.input_variable')}>
        <Select
          value={task.input_var || ''}
          onChange={e => onChange({ input_var: e.target.value })}
          options={inputVarOptions}
          placeholder={t('workflow.input_variable')}
        />
      </FormField>
    </>
  );
}
