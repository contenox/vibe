import { Button, FormField, H2, Input, P } from '@contenox/ui';
import { useEffect, useId, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { usePutCLIConfig } from '../../../hooks/usePutCLIConfig';
import type { SetupStatus } from '../../../lib/types';

type Props = {
  data: SetupStatus | undefined;
  onRefreshStatus: () => void;
};

export default function StepSetModel({ data, onRefreshStatus }: Props) {
  const { t } = useTranslation();
  const putConfig = usePutCLIConfig();
  const formId = useId();

  const allModels = (data?.backendChecks ?? []).flatMap(b => b.chatModels ?? []);
  const hasModels = allModels.length > 0;
  const hasDefaults = !!(data?.defaultModel || data?.defaultProvider);

  const [model, setModel] = useState(data?.defaultModel ?? '');
  const [provider, setProvider] = useState(data?.defaultProvider ?? '');
  const [defaultChain, setDefaultChain] = useState(data?.defaultChain ?? '');

  useEffect(() => {
    setModel(data?.defaultModel ?? '');
    setProvider(data?.defaultProvider ?? '');
    setDefaultChain(data?.defaultChain ?? '');
  }, [data]);

  useEffect(() => {
    if (putConfig.isSuccess) {
      onRefreshStatus();
      const timer = window.setTimeout(() => putConfig.reset(), 4000);
      return () => window.clearTimeout(timer);
    }
  }, [putConfig.isSuccess, putConfig.reset, onRefreshStatus]);

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const body: { 'default-model'?: string; 'default-provider'?: string; 'default-chain'?: string } = {};
    if (model.trim()) body['default-model'] = model.trim();
    if (provider.trim()) body['default-provider'] = provider.trim();
    if (defaultChain.trim()) body['default-chain'] = defaultChain.trim();
    if (!Object.keys(body).length) return;
    putConfig.mutate(body);
  };

  return (
    <div className="max-w-xl mx-auto space-y-6">
      <div className="space-y-1">
        <H2 className="text-xl font-semibold">{t('onboarding.step_set_model.title')}</H2>
        <P variant="muted" className="text-sm">
          {t('onboarding.step_set_model.desc')}
        </P>
      </div>

      {!hasModels && !hasDefaults && (
        <P variant="muted" className="text-sm">
          {t('onboarding.step_set_model.no_models')}
        </P>
      )}

      <form id={formId} onSubmit={handleSubmit} className="space-y-4">
        <FormField label={t('onboarding.step_set_model.model_label')}>
          {hasModels ? (
            <select
              className="w-full rounded border px-3 py-2 text-sm bg-background"
              value={model}
              onChange={e => setModel(e.target.value)}>
              <option value="">{t('onboarding.step_set_model.model_placeholder')}</option>
              {allModels.map(m => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </select>
          ) : (
            <Input
              value={model}
              onChange={e => setModel(e.target.value)}
              placeholder={t('onboarding.step_set_model.model_placeholder')}
            />
          )}
        </FormField>

        <FormField label={t('onboarding.step_set_model.provider_label')}>
          <Input
            value={provider}
            onChange={e => setProvider(e.target.value)}
            placeholder={t('onboarding.step_set_model.provider_placeholder')}
          />
        </FormField>

        <FormField label={t('onboarding.step_set_model.chain_label')}>
          <Input
            value={defaultChain}
            onChange={e => setDefaultChain(e.target.value)}
            placeholder={t('onboarding.step_set_model.chain_placeholder')}
          />
          <P variant="muted" className="text-xs mt-1">
            {t('onboarding.step_set_model.chain_help')}
          </P>
        </FormField>

        {putConfig.isError && (
          <P className="text-destructive text-sm">{putConfig.error.message}</P>
        )}
        {putConfig.isSuccess && (
          <P variant="muted" className="text-sm">{t('onboarding.step_set_model.saved')}</P>
        )}

        <Button type="submit" variant="primary" disabled={putConfig.isPending}>
          {t('onboarding.step_set_model.save')}
        </Button>
      </form>
    </div>
  );
}
