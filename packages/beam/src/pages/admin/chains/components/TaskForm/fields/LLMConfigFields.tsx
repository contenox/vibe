import { Collapsible, Input, Label, P, Select } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import { ExecuteConfig, FormTask } from '../../../../../../lib/types';

interface LLMConfigFieldsProps {
  task: FormTask;
  onChange: (updates: Partial<FormTask>) => void;
  expanded?: boolean;
}

export default function LLMConfigFields({
  task,
  onChange,
  expanded = false,
}: LLMConfigFieldsProps) {
  const { t } = useTranslation();
  const config: ExecuteConfig = task.execute_config || {};

  const updateConfig = (updates: Partial<ExecuteConfig>) => {
    onChange({
      execute_config: {
        ...config,
        ...updates,
      },
    });
  };

  const handleModelChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    updateConfig({ model: e.target.value || undefined });
  };

  const handleTemperatureChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const value = e.target.value;
    updateConfig({ temperature: value === '' ? undefined : Number(value) });
  };

  const handleProviderChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    updateConfig({ provider: e.target.value || undefined });
  };

  const handlePassClientsToolsChange = (e: React.ChangeEvent<HTMLSelectElement>) => {
    updateConfig({ pass_clients_tools: e.target.value === 'true' });
  };

  return (
    <Collapsible
      title={t('chains.task_form.advanced_llm_settings')}
      defaultExpanded={expanded}
      className="mt-4">
      <div className="space-y-4 pt-2">
        <div className="grid grid-cols-2 gap-4">
          <div>
            <Label className="block text-sm font-medium">{t('chains.task_form.model')}</Label>
            <Input
              value={config.model || ''}
              onChange={handleModelChange}
              placeholder="mistral:instruct"
            />
            <P className="text-text-muted mt-1 text-xs">{t('chains.task_form.model_help')}</P>
          </div>

          <div>
            <Label className="block text-sm font-medium">{t('chains.task_form.temperature')}</Label>
            <Input
              type="number"
              min={0}
              max={2}
              step={0.1}
              value={config.temperature ?? ''}
              onChange={handleTemperatureChange}
              placeholder="0.7"
            />
            <P className="text-text-muted mt-1 text-xs">{t('chains.task_form.temperature_help')}</P>
          </div>
        </div>

        <div className="grid grid-cols-2 gap-4">
          <div>
            <Label className="block text-sm font-medium">{t('chains.task_form.provider')}</Label>
            <Input
              value={config.provider || ''}
              onChange={handleProviderChange}
              placeholder="ollama"
            />
            <P className="text-text-muted mt-1 text-xs">{t('chains.task_form.provider_help')}</P>
          </div>

          <div>
            <Label className="block text-sm font-medium">
              {t('chains.task_form.pass_clients_tools')}
            </Label>
            <Select
              value={config.pass_clients_tools ? 'true' : 'false'}
              onChange={handlePassClientsToolsChange}
              options={[
                { value: 'false', label: t('common.no') },
                { value: 'true', label: t('common.yes') },
              ]}
            />
            <P className="text-text-muted mt-1 text-xs">
              {t('chains.task_form.pass_clients_tools_help')}
            </P>
          </div>
        </div>
      </div>
    </Collapsible>
  );
}
