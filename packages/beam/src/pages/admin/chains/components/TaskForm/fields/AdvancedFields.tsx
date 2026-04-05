import { Collapsible, Input, Label, P, Textarea } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import { FormTask } from '../../../../../../lib/types';

interface AdvancedFieldsProps {
  task: FormTask;
  onChange: (updates: Partial<FormTask>) => void;
}

export default function AdvancedFields({ task, onChange }: AdvancedFieldsProps) {
  const { t } = useTranslation();

  const handlePrintChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    onChange({ print: e.target.value });
  };

  const handleTimeoutChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    onChange({ timeout: e.target.value });
  };

  const handleRetryCountChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const value = e.target.value;
    onChange({ retry_on_failure: Number(value) || 0 });
  };

  return (
    <div className="space-y-6">
      {/* print */}
      <Collapsible title={t('chains.task_form.print_statement')} defaultExpanded={false}>
        <P className="text-text-muted mb-2 text-sm">{t('chains.task_form.print_statement_help')}</P>
        <Textarea
          rows={3}
          value={task.print || ''}
          onChange={handlePrintChange}
          placeholder="Result: {{.output}} - Status: {{.status}}"
          className="min-h-[80px] font-mono text-sm"
        />
      </Collapsible>

      {/* execution */}
      <Collapsible title={t('chains.task_form.execution_settings')} defaultExpanded={false}>
        <P className="text-text-muted mb-2 text-sm">
          {t('chains.task_form.execution_settings_help')}
        </P>
        <div className="grid grid-cols-2 gap-4">
          <div>
            <Label className="block text-sm font-medium">{t('chains.task_form.timeout')}</Label>
            <Input value={task.timeout || ''} onChange={handleTimeoutChange} placeholder="30s" />
            <P className="text-text-muted mt-1 text-xs">{t('chains.task_form.timeout_help')}</P>
          </div>

          <div>
            <label className="block text-sm font-medium">{t('chains.task_form.retry_count')}</label>
            <Input
              type="number"
              min={0}
              max={10}
              value={task.retry_on_failure ?? 0}
              onChange={handleRetryCountChange}
            />
            <P className="text-text-muted mt-1 text-xs">{t('chains.task_form.retry_count_help')}</P>
          </div>
        </div>
      </Collapsible>
    </div>
  );
}
