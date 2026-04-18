import { Button, H1, P } from '@contenox/ui';
import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useQueryClient } from '@tanstack/react-query';
import { setupKeys } from '../../lib/queryKeys';
import { cn } from '../../lib/utils';
import type { SetupStatus } from '../../lib/types';
import StepChooseProvider, { type ProviderChoice } from './steps/StepChooseProvider';
import StepGetModel from './steps/StepGetModel';
import StepSetModel from './steps/StepSetModel';
import StepHealthCheck from './steps/StepHealthCheck';

const TOTAL_STEPS = 4;

function detectProvider(provider: string): ProviderChoice {
  const p = provider.trim().toLowerCase();
  if (p === 'local') return 'local';
  if (p === 'ollama') return 'ollama';
  if (p === 'openai') return 'openai';
  if (p === 'gemini') return 'gemini';
  if (p.startsWith('vertex')) return 'vertex';
  return 'local';
}

type Props = {
  data: SetupStatus | undefined;
  onDismiss: () => void;
};

export function OnboardingWizard({ data, onDismiss }: Props) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [step, setStep] = useState(0);
  const [provider, setProvider] = useState<ProviderChoice>('local');
  const [initialized, setInitialized] = useState(false);

  // Smart starting step — runs once after first data load
  useEffect(() => {
    if (!data || initialized) return;
    setInitialized(true);
    if (data.defaultProvider) {
      setProvider(detectProvider(data.defaultProvider));
    }
    if (data.backendCount > 0) {
      setStep(3); // already has backends → health check
    } else if (data.defaultProvider || data.defaultModel) {
      setStep(1); // defaults configured but no backends → get a model
    }
    // else: step 0 — fresh start
  }, [data, initialized]);

  const refreshStatus = () => {
    void queryClient.invalidateQueries({ queryKey: setupKeys.status() });
  };

  // Check if everything is healthy — offer finish shortcut
  const isHealthy = useMemo(() => {
    if (!data) return false;
    const hasErrors = (data.issues ?? []).some(i => i.severity === 'error');
    return !hasErrors && data.reachableBackendCount > 0;
  }, [data]);

  return (
    <div className="flex flex-col h-full overflow-hidden">
      {/* Header */}
      <div className="shrink-0 border-b px-6 py-4 flex items-center gap-4">
        <div className="flex-1 min-w-0">
          <H1 className="text-lg font-semibold leading-tight">{t('onboarding.title')}</H1>
          <P variant="muted" className="text-xs mt-0.5">
            {t('onboarding.step_of', { current: step + 1, total: TOTAL_STEPS })}
          </P>
        </div>
        <div className="flex items-center gap-1.5 max-w-xs flex-1">
          {Array.from({ length: TOTAL_STEPS }, (_, i) => (
            <div
              key={i}
              className={cn(
                'h-1.5 flex-1 rounded-full transition-colors',
                i === step
                  ? 'bg-primary-600 dark:bg-primary-500'
                  : i < step
                    ? 'bg-primary-400/70 dark:bg-primary-600/60'
                    : 'bg-surface-200 dark:bg-dark-surface-600',
              )}
            />
          ))}
        </div>
        <Button variant="ghost" size="sm" onClick={onDismiss}>
          {t('onboarding.dismiss')}
        </Button>
      </div>

      {/* Step content */}
      <div className="flex-1 min-h-0 overflow-auto p-6">
        {step === 0 && <StepChooseProvider value={provider} onChange={setProvider} />}
        {step === 1 && <StepGetModel provider={provider} onRefreshStatus={refreshStatus} />}
        {step === 2 && <StepSetModel data={data} onRefreshStatus={refreshStatus} />}
        {step === 3 && (
          <StepHealthCheck data={data} onRefreshStatus={refreshStatus} onFinish={onDismiss} />
        )}
      </div>

      {/* Footer navigation */}
      <div className="shrink-0 border-t px-6 py-4 flex items-center justify-between">
        <Button
          variant="secondary"
          size="sm"
          onClick={() => setStep(s => Math.max(0, s - 1))}
          disabled={step === 0}>
          {t('onboarding.back')}
        </Button>
        <div className="flex items-center gap-2">
          {isHealthy && step < TOTAL_STEPS - 1 && (
            <Button variant="secondary" size="sm" onClick={onDismiss}>
              {t('onboarding.finish')}
            </Button>
          )}
          {step < TOTAL_STEPS - 1 ? (
            <Button variant="primary" size="sm" onClick={() => setStep(s => s + 1)}>
              {t('onboarding.continue')}
            </Button>
          ) : (
            <Button variant="primary" size="sm" onClick={onDismiss}>
              {t('onboarding.finish')}
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}
