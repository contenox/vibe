import { Accordion, Button, Input, P, Panel, Spinner, Wizard } from '@contenox/ui';
import React, { useContext, useEffect, useId, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useQueryClient } from '@tanstack/react-query';
import { useNavigate } from 'react-router-dom';
import { usePutCLIConfig } from '../../hooks/usePutCLIConfig';
import { useSetupStatus } from '../../hooks/useSetupStatus';
import { AuthContext } from '../../lib/authContext';
import { setupKeys } from '../../lib/queryKeys';
import { deriveSetupWizardSteps, getRecommendedSetupStepIndex } from '../../lib/setupWizard';
import { cn } from '../../lib/utils';

const STORAGE_KEY = 'beam_setup_wizard_dismiss';
const TOTAL_STEPS = 3;

export type SetupWizardFlowVariant = 'banner' | 'page';

function issueSignature(issueCodes: string[]) {
  return [...issueCodes].sort().join(',');
}

function providerKind(provider: string): 'openai' | 'gemini' | 'local' {
  const p = provider.trim().toLowerCase();
  if (p === 'openai') return 'openai';
  if (p === 'gemini') return 'gemini';
  return 'local';
}

type SetupWizardFlowProps = {
  variant: SetupWizardFlowVariant;
};

/**
 * Shared setup steps (defaults → backends → health). Used in the layout banner and on `/settings`.
 */
export function SetupWizardFlow({ variant }: SetupWizardFlowProps) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { user } = useContext(AuthContext);
  const { data, isLoading, isError, isFetching, error } = useSetupStatus(!!user);
  const putConfig = usePutCLIConfig();
  const defaultsFormId = useId();

  const [model, setModel] = useState('');
  const [provider, setProvider] = useState('');
  const [defaultChain, setDefaultChain] = useState('');
  const [dismissedSig, setDismissedSig] = useState<string | null>(null);
  const [currentStep, setCurrentStep] = useState(0);
  const lastSyncedSigRef = useRef<string | null>(null);

  const sig = useMemo(() => {
    const issues = data?.issues;
    if (!issues?.length) return '';
    return issueSignature(issues.map(i => i.code));
  }, [data]);

  useEffect(() => {
    if (!data) return;
    setModel(data.defaultModel || '');
    setProvider(data.defaultProvider || '');
    setDefaultChain(data.defaultChain || '');
  }, [data]);

  useEffect(() => {
    try {
      setDismissedSig(localStorage.getItem(STORAGE_KEY));
    } catch {
      setDismissedSig(null);
    }
  }, [sig]);

  useEffect(() => {
    if (!data || sig === '') return;
    if (lastSyncedSigRef.current === sig) return;
    lastSyncedSigRef.current = sig;
    setCurrentStep(getRecommendedSetupStepIndex(data));
  }, [data, sig]);

  const issueList = data?.issues ?? [];

  const bannerVisible =
    !!user && !isLoading && !isError && !!data && issueList.length > 0 && dismissedSig !== sig;

  const pageVisible = !!user && !isLoading && !isError && !!data;

  const visible = variant === 'page' ? pageVisible : bannerVisible;

  const hasBlockingIssue = !!issueList.some(i => i.severity === 'error');

  const steps = useMemo(() => (data ? deriveSetupWizardSteps(data) : []), [data]);

  const defaultIssues = issueList.filter(i => i.category === 'defaults');
  const registrationIssues = issueList.filter(i => i.category === 'registration');
  const healthIssues = issueList.filter(i => i.category === 'health');
  const registrationTarget = registrationIssues[0]?.fixPath;
  const healthTarget = healthIssues[0]?.fixPath;
  const registrationCommands = registrationIssues.filter(i => i.cliCommand);
  const healthCommands = healthIssues.filter(i => i.cliCommand);
  const diagnosticChecks = (data?.backendChecks ?? []).filter(
    check => check.defaultProvider || check.status !== 'ready',
  );

  const dismiss = () => {
    try {
      localStorage.setItem(STORAGE_KEY, sig);
    } catch {
      /* ignore */
    }
    setDismissedSig(sig);
  };

  useEffect(() => {
    if (!putConfig.isSuccess) return;
    const timer = window.setTimeout(() => putConfig.reset(), 4000);
    return () => window.clearTimeout(timer);
  }, [putConfig.isSuccess, putConfig.reset]);

  const saveAndContinue = (e: React.FormEvent) => {
    e.preventDefault();
    putConfig.reset();
    const body: {
      'default-model'?: string;
      'default-provider'?: string;
      'default-chain'?: string;
    } = {};
    if (model.trim()) body['default-model'] = model.trim();
    if (provider.trim()) body['default-provider'] = provider.trim();
    if (defaultChain.trim()) body['default-chain'] = defaultChain.trim();
    if (!body['default-model'] && !body['default-provider'] && !body['default-chain']) return;
    putConfig.mutate(body, {
      onSuccess: () => {
        if (variant === 'banner') setCurrentStep(1);
      },
    });
  };

  const kind = providerKind(provider || (data?.defaultProvider ?? ''));
  const backendsTabLink =
    kind === 'openai' || kind === 'gemini'
      ? '/backends?tab=cloud-providers'
      : '/backends?tab=backends';
  const registerLink = registrationTarget || backendsTabLink;
  const registrationMode = registerLink.includes('cloud-providers') ? 'cloud' : 'local';
  const healthLink = healthTarget || backendsTabLink;

  const refreshStatus = () => {
    void queryClient.invalidateQueries({ queryKey: setupKeys.status() });
  };

  if (variant === 'page') {
    if (!user) return null;
    if (isLoading) {
      return (
        <div className="flex justify-center py-12">
          <Spinner size="md" />
        </div>
      );
    }
    if (isError) {
      return (
        <P className="text-destructive text-sm">
          {(error as Error)?.message ?? t('settings.setup_load_error')}
        </P>
      );
    }
  }

  if (!visible || !data) return null;

  const [dStep] = steps;

  /** On the Settings page, always show default fields so users can edit even when status is "complete". */
  const showDefaultsForm = variant === 'page' || dStep.status !== 'complete';

  const stepTitle =
    currentStep === 0
      ? t('setup.wizard.step_defaults_title')
      : currentStep === 1
        ? registrationMode === 'cloud'
          ? t('setup.wizard.step_register_title_cloud')
          : t('setup.wizard.step_register_title_local')
        : t('setup.wizard.step_health_title');

  const stepDescription =
    currentStep === 0
      ? t('setup.wizard.step_defaults_desc')
      : currentStep === 1
        ? registrationMode === 'cloud'
          ? t('setup.wizard.step_register_desc_cloud')
          : t('setup.wizard.step_register_desc_local')
        : t('setup.wizard.step_health_desc');

  const wizardTitle = hasBlockingIssue
    ? t('setup.wizard.title_blocking')
    : t('setup.wizard.title_warnings');
  const wizardIntro = t('setup.wizard.intro');

  const footerActions = (
    <div className="flex w-full flex-wrap items-center justify-between gap-3">
      {variant === 'banner' ? (
        <Button type="button" variant="secondary" size="sm" onClick={dismiss}>
          {t('setup.dismiss')}
        </Button>
      ) : (
        <span />
      )}
      <div className="flex flex-wrap items-center justify-end gap-2">
        {currentStep > 0 && (
          <Button
            type="button"
            variant="secondary"
            size="sm"
            onClick={() => setCurrentStep(s => Math.max(0, s - 1))}>
            {t('setup.wizard.back')}
          </Button>
        )}
        {currentStep === 0 &&
          (dStep.status === 'complete' && !showDefaultsForm ? (
            <Button type="button" variant="primary" size="sm" onClick={() => setCurrentStep(1)}>
              {t('setup.wizard.continue_to_register')}
            </Button>
          ) : (
            <>
              <Button
                type="submit"
                form={defaultsFormId}
                variant="primary"
                size="sm"
                disabled={putConfig.isPending}>
                {variant === 'page' ? t('setup.save') : t('setup.wizard.save_and_continue')}
              </Button>
              {variant === 'page' && dStep.status === 'complete' && (
                <Button
                  type="button"
                  variant="secondary"
                  size="sm"
                  onClick={() => setCurrentStep(1)}>
                  {t('setup.wizard.continue_to_register')}
                </Button>
              )}
            </>
          ))}
        {currentStep === 1 && (
          <Button type="button" variant="primary" size="sm" onClick={() => setCurrentStep(2)}>
            {t('setup.wizard.continue_to_health')}
          </Button>
        )}
        {currentStep === 2 && (
          <Button
            type="button"
            variant="secondary"
            size="sm"
            onClick={refreshStatus}
            disabled={isFetching}>
            {t('setup.wizard.refresh_status')}
          </Button>
        )}
      </div>
    </div>
  );

  const progressAndBody = (
    <div
      className="space-y-4"
      role="region"
      aria-label={t('setup.wizard.progress_label')}
      aria-live="polite">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:gap-4">
        <P variant="muted" className="text-sm font-medium whitespace-nowrap">
          {t('setup.wizard.step_of', { current: currentStep + 1, total: TOTAL_STEPS })}
        </P>
        <div className="flex min-h-2 flex-1 gap-1.5" aria-hidden>
          {Array.from({ length: TOTAL_STEPS }, (_, i) => (
            <div
              key={i}
              className={cn(
                'h-2 flex-1 rounded-full transition-colors',
                i === currentStep
                  ? 'bg-primary-600 dark:bg-primary-500'
                  : i < currentStep
                    ? 'bg-emerald-500/80 dark:bg-emerald-600/70'
                    : 'bg-surface-200 dark:bg-dark-surface-600',
              )}
            />
          ))}
        </div>
      </div>

      <div className="border-surface-200 dark:border-dark-surface-600 rounded-lg border bg-inherit p-4">
        <h4 className="text-text dark:text-dark-text text-base font-semibold">{stepTitle}</h4>
        {stepDescription ? (
          <P variant="muted" className="mt-1 text-sm">
            {stepDescription}
          </P>
        ) : null}

        <div className="mt-4 space-y-3">
          {currentStep === 0 && (
            <>
              {defaultIssues.length > 0 ? (
                <ul className="text-muted-foreground list-inside list-disc space-y-1 text-sm">
                  {defaultIssues.map(iss => (
                    <li key={iss.code}>{iss.message}</li>
                  ))}
                </ul>
              ) : null}
              {showDefaultsForm ? (
                <form
                  id={defaultsFormId}
                  onSubmit={saveAndContinue}
                  className="flex max-w-md flex-col gap-2">
                  <Input
                    name="default-model"
                    placeholder={t('setup.model_placeholder')}
                    value={model}
                    onChange={e => setModel(e.target.value)}
                    aria-label={t('setup.model_placeholder')}
                  />
                  <Input
                    name="default-provider"
                    placeholder={t('setup.provider_placeholder')}
                    value={provider}
                    onChange={e => setProvider(e.target.value)}
                    aria-label={t('setup.provider_placeholder')}
                  />
                  <Input
                    name="default-chain"
                    placeholder={t('setup.default_chain_placeholder')}
                    value={defaultChain}
                    onChange={e => setDefaultChain(e.target.value)}
                    aria-label={t('setup.default_chain_placeholder')}
                  />
                  <P className="text-muted-foreground text-xs">{t('setup.default_chain_help')}</P>
                  {putConfig.isError ? (
                    <P className="text-destructive text-sm">{putConfig.error.message}</P>
                  ) : null}
                  {putConfig.isSuccess ? (
                    <P className="text-muted-foreground text-sm">{t('setup.saved')}</P>
                  ) : null}
                </form>
              ) : (
                <P className="text-muted-foreground text-sm">{t('setup.wizard.defaults_done_hint')}</P>
              )}
            </>
          )}

          {currentStep === 1 && (
            <>
              <div className="flex flex-wrap gap-2">
                <Button
                  type="button"
                  variant="primary"
                  size="sm"
                  onClick={() => navigate(registerLink)}>
                  {registerLink.includes('cloud-providers')
                    ? t('setup.wizard.cta_cloud_providers')
                    : t('setup.wizard.cta_backends_tab')}
                </Button>
                <Button
                  type="button"
                  variant="secondary"
                  size="sm"
                  onClick={() => navigate('/backends')}>
                  {t('setup.wizard.cta_backends_all')}
                </Button>
              </div>
              {registrationIssues.length > 0 ? (
                <ul className="text-muted-foreground mt-2 list-inside list-disc space-y-1 text-sm">
                  {registrationIssues.map(issue => (
                    <li key={issue.code}>{issue.message}</li>
                  ))}
                </ul>
              ) : null}
              {registrationCommands.length > 0 ? (
                <Accordion title={t('setup.wizard.cli_advanced')} className="mt-2 max-w-xl">
                  <div className="space-y-2">
                    {registrationCommands.map(issue => (
                      <P key={issue.code} className="text-muted-foreground font-mono text-xs">
                        {issue.cliCommand}
                      </P>
                    ))}
                  </div>
                </Accordion>
              ) : null}
            </>
          )}

          {currentStep === 2 && (
            <>
              {healthIssues.length > 0 ? (
                <ul className="text-destructive list-inside list-disc space-y-1 text-sm">
                  {healthIssues.map(issue => (
                    <li key={issue.code}>{issue.message}</li>
                  ))}
                </ul>
              ) : null}
              {healthCommands.length > 0 ? (
                <Accordion title={t('setup.wizard.cli_advanced')} className="mt-2 max-w-xl">
                  <div className="space-y-2">
                    {healthCommands.map(issue => (
                      <P key={issue.code} className="text-muted-foreground font-mono text-xs">
                        {issue.cliCommand}
                      </P>
                    ))}
                  </div>
                </Accordion>
              ) : null}
              {diagnosticChecks.length > 0 ? (
                <ul className="text-muted-foreground mt-3 list-inside list-disc space-y-1 text-sm">
                  {diagnosticChecks.map(check => (
                    <li key={check.id}>
                      <span className="font-medium">
                        {check.defaultProvider ? '[default] ' : ''}
                        {check.name}
                      </span>
                      {`: ${check.type}`}
                      {check.error ? ` - ${check.error}` : ''}
                      {!check.error && check.status === 'ready'
                        ? ` - ${check.chatModelCount} chat model(s)${
                            check.chatModels?.length ? ` (${check.chatModels.join(', ')})` : ''
                          }`
                        : ''}
                      {check.hint ? ` (${check.hint})` : ''}
                    </li>
                  ))}
                </ul>
              ) : null}
              {data.backendCount > 0 &&
              data.reachableBackendCount === 0 &&
              healthIssues.length === 0 ? (
                <P className="text-muted-foreground text-sm">{t('setup.wizard.health_pending')}</P>
              ) : null}
              {data.backendCount > 0 &&
              data.reachableBackendCount > 0 &&
              healthIssues.length === 0 ? (
                <P className="text-muted-foreground text-sm">
                  {t('setup.wizard.health_ok', {
                    reachable: data.reachableBackendCount,
                    total: data.backendCount,
                  })}
                </P>
              ) : null}
              <Button
                type="button"
                variant="secondary"
                size="sm"
                onClick={() => navigate(healthLink)}>
                {healthLink.includes('cloud-providers')
                  ? t('setup.wizard.cta_cloud_providers')
                  : t('setup.wizard.cta_check_backends')}
              </Button>
            </>
          )}
        </div>
      </div>
    </div>
  );

  if (variant === 'page') {
    return (
      <Panel
        variant="bordered"
        className="bg-surface dark:bg-dark-surface-100 border-surface-200 dark:border-dark-surface-600">
        <header className="mb-4 space-y-1">
          <h2 className="text-text dark:text-dark-text text-lg font-semibold">{wizardTitle}</h2>
          <P variant="muted" className="text-sm">
            {wizardIntro}
          </P>
        </header>
        {progressAndBody}
        <footer className="border-surface-200 dark:border-dark-surface-600 mt-4 border-t pt-4">
          {footerActions}
        </footer>
      </Panel>
    );
  }

  return (
    <Wizard
      title={wizardTitle}
      description={wizardIntro}
      footer={footerActions}
      className="shrink-0">
      {progressAndBody}
    </Wizard>
  );
}
