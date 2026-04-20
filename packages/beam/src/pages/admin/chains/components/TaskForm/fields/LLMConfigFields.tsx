import { Collapsible, Input, Label, P, Select } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import {
  CompactPolicy,
  ExecuteConfig,
  FormTask,
  RetryPolicy,
} from '../../../../../../lib/types';
import HookPoliciesFields from './HookPoliciesFields';

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

  // Retry policy edits only the nested retry_policy object. A missing
  // retry_policy disables retry on the backend (zero value), so we omit the
  // whole sub-object when every field is cleared.
  const retry: RetryPolicy = config.retry_policy || {};
  const updateRetry = (updates: Partial<RetryPolicy>) => {
    const merged: RetryPolicy = { ...retry, ...updates };
    const allEmpty =
      !merged.max_attempts &&
      !merged.initial_backoff &&
      !merged.max_backoff &&
      !merged.jitter &&
      !merged.rate_limit_min_wait &&
      !merged.fallback_model_id &&
      !merged.fallback_after;
    updateConfig({ retry_policy: allEmpty ? undefined : merged });
  };

  const compact: CompactPolicy = config.compact_policy || {};
  const updateCompact = (updates: Partial<CompactPolicy>) => {
    const merged: CompactPolicy = { ...compact, ...updates };
    const allEmpty =
      !merged.trigger_fraction &&
      !merged.keep_recent &&
      !merged.model &&
      !merged.provider &&
      !merged.max_failures &&
      !merged.min_replaced_messages;
    updateConfig({ compact_policy: allEmpty ? undefined : merged });
  };

  const numOrUndef = (v: string): number | undefined =>
    v === '' ? undefined : Number(v);
  const strOrUndef = (v: string): string | undefined => (v === '' ? undefined : v);

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

        <div className="grid grid-cols-2 gap-4">
          <div>
            <Label className="block text-sm font-medium">{t('chains.task_form.think')}</Label>
            <Select
              value={config.think ?? ''}
              onChange={e => updateConfig({ think: e.target.value || undefined })}
              options={[
                { value: '', label: t('chains.task_form.think_default') },
                { value: 'low', label: t('chains.task_form.think_low') },
                { value: 'medium', label: t('chains.task_form.think_medium') },
                { value: 'high', label: t('chains.task_form.think_high') },
              ]}
            />
            <P className="text-text-muted mt-1 text-xs">{t('chains.task_form.think_help')}</P>
          </div>

          <div>
            <Label className="block text-sm font-medium">{t('chains.task_form.shift')}</Label>
            <Select
              value={config.shift ? 'true' : 'false'}
              onChange={e => updateConfig({ shift: e.target.value === 'true' || undefined })}
              options={[
                { value: 'false', label: t('common.no') },
                { value: 'true', label: t('common.yes') },
              ]}
            />
            <P className="text-text-muted mt-1 text-xs">{t('chains.task_form.shift_help')}</P>
          </div>
        </div>

        <div className="grid grid-cols-2 gap-4">
          <div>
            <Label className="block text-sm font-medium">{t('chains.task_form.hooks')}</Label>
            <Input
              value={(config.hooks ?? []).join(', ')}
              onChange={e => {
                const val = e.target.value.trim();
                updateConfig({ hooks: val ? val.split(',').map(s => s.trim()).filter(Boolean) : undefined });
              }}
              placeholder="*, !hook_name, hook_a"
            />
            <P className="text-text-muted mt-1 text-xs">{t('chains.task_form.hooks_help')}</P>
          </div>

          <div>
            <Label className="block text-sm font-medium">{t('chains.task_form.hide_tools')}</Label>
            <Input
              value={(config.hide_tools ?? []).join(', ')}
              onChange={e => {
                const val = e.target.value.trim();
                updateConfig({ hide_tools: val ? val.split(',').map(s => s.trim()).filter(Boolean) : undefined });
              }}
              placeholder="tool1, hook.tool2"
            />
            <P className="text-text-muted mt-1 text-xs">{t('chains.task_form.hide_tools_help')}</P>
          </div>
        </div>

        <Collapsible
          title={t('chains.task_form.retry_policy')}
          defaultExpanded={false}
          className="mt-2">
          <div className="space-y-3 pt-2">
            <P className="text-text-muted text-xs">{t('chains.task_form.retry_policy_help')}</P>
            <div className="grid grid-cols-2 gap-4">
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.retry_max_attempts')}
                </Label>
                <Input
                  type="number"
                  min={0}
                  step={1}
                  value={retry.max_attempts ?? ''}
                  onChange={(e) => updateRetry({ max_attempts: numOrUndef(e.target.value) })}
                  placeholder="4"
                />
                <P className="text-text-muted mt-1 text-xs">
                  {t('chains.task_form.retry_max_attempts_help')}
                </P>
              </div>
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.retry_jitter')}
                </Label>
                <Input
                  type="number"
                  min={0}
                  max={1}
                  step={0.05}
                  value={retry.jitter ?? ''}
                  onChange={(e) => updateRetry({ jitter: numOrUndef(e.target.value) })}
                  placeholder="0.25"
                />
                <P className="text-text-muted mt-1 text-xs">
                  {t('chains.task_form.retry_jitter_help')}
                </P>
              </div>
            </div>
            <div className="grid grid-cols-2 gap-4">
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.retry_initial_backoff')}
                </Label>
                <Input
                  value={retry.initial_backoff || ''}
                  onChange={(e) => updateRetry({ initial_backoff: strOrUndef(e.target.value) })}
                  placeholder="1s"
                />
                <P className="text-text-muted mt-1 text-xs">
                  {t('chains.task_form.retry_initial_backoff_help')}
                </P>
              </div>
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.retry_max_backoff')}
                </Label>
                <Input
                  value={retry.max_backoff || ''}
                  onChange={(e) => updateRetry({ max_backoff: strOrUndef(e.target.value) })}
                  placeholder="30s"
                />
                <P className="text-text-muted mt-1 text-xs">
                  {t('chains.task_form.retry_max_backoff_help')}
                </P>
              </div>
            </div>
            <div>
              <Label className="block text-sm font-medium">
                {t('chains.task_form.retry_rate_limit_min_wait')}
              </Label>
              <Input
                value={retry.rate_limit_min_wait || ''}
                onChange={(e) =>
                  updateRetry({ rate_limit_min_wait: strOrUndef(e.target.value) })
                }
                placeholder="10s"
              />
              <P className="text-text-muted mt-1 text-xs">
                {t('chains.task_form.retry_rate_limit_min_wait_help')}
              </P>
            </div>
            <div className="grid grid-cols-2 gap-4">
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.retry_fallback_model_id')}
                </Label>
                <Input
                  value={retry.fallback_model_id || ''}
                  onChange={(e) =>
                    updateRetry({ fallback_model_id: strOrUndef(e.target.value) })
                  }
                  placeholder="gpt-4o-mini"
                />
                <P className="text-text-muted mt-1 text-xs">
                  {t('chains.task_form.retry_fallback_model_id_help')}
                </P>
              </div>
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.retry_fallback_after')}
                </Label>
                <Input
                  type="number"
                  min={0}
                  step={1}
                  value={retry.fallback_after ?? ''}
                  onChange={(e) => updateRetry({ fallback_after: numOrUndef(e.target.value) })}
                  placeholder="3"
                />
                <P className="text-text-muted mt-1 text-xs">
                  {t('chains.task_form.retry_fallback_after_help')}
                </P>
              </div>
            </div>
          </div>
        </Collapsible>

        <Collapsible
          title={t('chains.task_form.compact_policy')}
          defaultExpanded={false}
          className="mt-2">
          <div className="space-y-3 pt-2">
            <P className="text-text-muted text-xs">{t('chains.task_form.compact_policy_help')}</P>
            <div className="grid grid-cols-2 gap-4">
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.compact_trigger_fraction')}
                </Label>
                <Input
                  type="number"
                  min={0}
                  max={1}
                  step={0.05}
                  value={compact.trigger_fraction ?? ''}
                  onChange={(e) =>
                    updateCompact({ trigger_fraction: numOrUndef(e.target.value) })
                  }
                  placeholder="0.85"
                />
                <P className="text-text-muted mt-1 text-xs">
                  {t('chains.task_form.compact_trigger_fraction_help')}
                </P>
              </div>
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.compact_keep_recent')}
                </Label>
                <Input
                  type="number"
                  min={0}
                  step={1}
                  value={compact.keep_recent ?? ''}
                  onChange={(e) => updateCompact({ keep_recent: numOrUndef(e.target.value) })}
                  placeholder="8"
                />
                <P className="text-text-muted mt-1 text-xs">
                  {t('chains.task_form.compact_keep_recent_help')}
                </P>
              </div>
            </div>
            <div className="grid grid-cols-2 gap-4">
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.compact_model')}
                </Label>
                <Input
                  value={compact.model || ''}
                  onChange={(e) => updateCompact({ model: strOrUndef(e.target.value) })}
                  placeholder="{{var:compact_model}}"
                />
                <P className="text-text-muted mt-1 text-xs">
                  {t('chains.task_form.compact_model_help')}
                </P>
              </div>
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.compact_provider')}
                </Label>
                <Input
                  value={compact.provider || ''}
                  onChange={(e) => updateCompact({ provider: strOrUndef(e.target.value) })}
                  placeholder="{{var:provider}}"
                />
                <P className="text-text-muted mt-1 text-xs">
                  {t('chains.task_form.compact_provider_help')}
                </P>
              </div>
            </div>
            <div className="grid grid-cols-2 gap-4">
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.compact_max_failures')}
                </Label>
                <Input
                  type="number"
                  min={0}
                  step={1}
                  value={compact.max_failures ?? ''}
                  onChange={(e) => updateCompact({ max_failures: numOrUndef(e.target.value) })}
                  placeholder="3"
                />
                <P className="text-text-muted mt-1 text-xs">
                  {t('chains.task_form.compact_max_failures_help')}
                </P>
              </div>
              <div>
                <Label className="block text-sm font-medium">
                  {t('chains.task_form.compact_min_replaced_messages')}
                </Label>
                <Input
                  type="number"
                  min={0}
                  step={1}
                  value={compact.min_replaced_messages ?? ''}
                  onChange={(e) =>
                    updateCompact({ min_replaced_messages: numOrUndef(e.target.value) })
                  }
                  placeholder="4"
                />
                <P className="text-text-muted mt-1 text-xs">
                  {t('chains.task_form.compact_min_replaced_messages_help')}
                </P>
              </div>
            </div>
          </div>
        </Collapsible>

        <HookPoliciesFields task={task} onChange={onChange} />
      </div>
    </Collapsible>
  );
}
