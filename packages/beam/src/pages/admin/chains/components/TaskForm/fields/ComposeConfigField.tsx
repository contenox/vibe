import { FormField, Select } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import type { BranchCompose } from '../../../../../../lib/types';

interface ComposeConfigFieldProps {
  config: BranchCompose | undefined;
  onChange: (config: BranchCompose) => void;
  availableVariables: string[];
}

const COMPOSE_STRATEGIES = [
  { value: 'override', label: 'Override' },
  { value: 'merge_chat_histories', label: 'Merge Chat Histories' },
  { value: 'append_string_to_chat_history', label: 'Append String to Chat History' },
] as const;

export default function ComposeConfigField({
  config,
  onChange,
  availableVariables,
}: ComposeConfigFieldProps) {
  const { t } = useTranslation();

  const safeConfig: BranchCompose = config ?? {};

  const strategyOptions = COMPOSE_STRATEGIES.map(s => ({
    value: s.value,
    label: s.label,
  }));

  const withVarOptions = availableVariables.map(v => ({ value: v, label: v }));

  const handleStrategyChange = (strategy: string) => {
    onChange({ ...safeConfig, strategy: strategy as BranchCompose['strategy'] });
  };

  const handleWithVarChange = (with_var: string) => {
    onChange({ ...safeConfig, with_var });
  };

  return (
    <div className="space-y-4 border-t pt-4">
      <h4 className="font-medium">{t('chains.compose_configuration')}</h4>

      <FormField label={t('chains.compose_strategy')}>
        <Select
          value={safeConfig.strategy || ''}
          onChange={e => handleStrategyChange(e.target.value)}
          options={strategyOptions}
          placeholder={t('chains.select_strategy')}
        />
      </FormField>

      <FormField label={t('chains.compose_with_variable')}>
        <Select
          value={safeConfig.with_var || ''}
          onChange={e => handleWithVarChange(e.target.value)}
          options={withVarOptions}
          placeholder={t('chains.select_variable')}
        />
      </FormField>

      {safeConfig.strategy && (
        <div className="text-muted-foreground text-sm">
          {safeConfig.strategy === 'override' && t('chains.compose_override_hint')}
          {safeConfig.strategy === 'merge_chat_histories' && t('chains.compose_merge_hint')}
          {safeConfig.strategy === 'append_string_to_chat_history' &&
            t('chains.compose_append_hint')}
        </div>
      )}
    </div>
  );
}
