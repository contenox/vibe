import { Accordion, Button, Input, P, Wizard, WizardStep } from '@contenox/ui';
import React, { useContext, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { usePutCLIConfig } from '../../hooks/usePutCLIConfig';
import { useSetupStatus } from '../../hooks/useSetupStatus';
import { AuthContext } from '../../lib/authContext';
import { deriveSetupWizardSteps } from '../../lib/setupWizard';

const STORAGE_KEY = 'beam_setup_wizard_dismiss';

function issueSignature(issueCodes: string[]) {
  return [...issueCodes].sort().join(',');
}

function providerKind(provider: string): 'openai' | 'gemini' | 'local' {
  const p = provider.trim().toLowerCase();
  if (p === 'openai') return 'openai';
  if (p === 'gemini') return 'gemini';
  return 'local';
}

export function SetupWizard() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { user } = useContext(AuthContext);
  const { data, isLoading, isError } = useSetupStatus(!!user);
  const putConfig = usePutCLIConfig();

  const [model, setModel] = useState('');
  const [provider, setProvider] = useState('');
  const [defaultChain, setDefaultChain] = useState('');
  const [dismissedSig, setDismissedSig] = useState<string | null>(null);

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

  const issueList = data?.issues ?? [];

  const visible =
    !!user && !isLoading && !isError && !!data && issueList.length > 0 && dismissedSig !== sig;

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

  const saveDefaults = (e: React.FormEvent) => {
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
    putConfig.mutate(body);
  };

  const kind = providerKind(provider || (data?.defaultProvider ?? ''));
  const backendsTabLink =
    kind === 'openai' || kind === 'gemini'
      ? '/backends?tab=cloud-providers'
      : '/backends?tab=backends';
  const registerLink = registrationTarget || backendsTabLink;
  const registrationMode = registerLink.includes('cloud-providers') ? 'cloud' : 'local';
  const healthLink = healthTarget || backendsTabLink;

  if (!visible || !data) return null;

  const [dStep, rStep, hStep] = steps;

  return (
    <Wizard
      title={hasBlockingIssue ? t('setup.wizard.title_blocking') : t('setup.wizard.title_warnings')}
      description={t('setup.wizard.intro')}
      footer={
        <div className="flex flex-wrap justify-end gap-2">
          <Button type="button" variant="secondary" size="sm" onClick={dismiss}>
            {t('setup.dismiss')}
          </Button>
        </div>
      }
      className="shrink-0">
      <div className="flex flex-col gap-6 lg:flex-row lg:items-start">
        <div className="min-w-0 flex-1 space-y-0">
          <WizardStep
            step={1}
            status={dStep.status}
            active={dStep.active}
            title={t('setup.wizard.step_defaults_title')}
            description={t('setup.wizard.step_defaults_desc')}
            isLast={false}>
            {defaultIssues && defaultIssues.length > 0 ? (
              <ul className="text-muted-foreground list-inside list-disc space-y-1 text-sm">
                {defaultIssues.map(iss => (
                  <li key={iss.code}>{iss.message}</li>
                ))}
              </ul>
            ) : null}
            <form onSubmit={saveDefaults} className="flex max-w-md flex-col gap-2">
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
              <div className="flex flex-wrap gap-2">
                <Button type="submit" variant="primary" size="sm" disabled={putConfig.isPending}>
                  {t('setup.save')}
                </Button>
              </div>
              {putConfig.isError ? (
                <P className="text-destructive text-sm">{putConfig.error.message}</P>
              ) : null}
              {putConfig.isSuccess ? (
                <P className="text-muted-foreground text-sm">{t('setup.saved')}</P>
              ) : null}
            </form>
          </WizardStep>

          <WizardStep
            step={2}
            status={rStep.status}
            active={rStep.active}
            title={
              registrationMode === 'cloud'
                ? t('setup.wizard.step_register_title_cloud')
                : t('setup.wizard.step_register_title_local')
            }
            description={
              registrationMode === 'cloud'
                ? t('setup.wizard.step_register_desc_cloud')
                : t('setup.wizard.step_register_desc_local')
            }
            isLast={false}>
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
          </WizardStep>

          <WizardStep
            step={3}
            status={hStep.status}
            active={hStep.active}
            title={t('setup.wizard.step_health_title')}
            description={t('setup.wizard.step_health_desc')}
            isLast>
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
          </WizardStep>
        </div>
      </div>
    </Wizard>
  );
}
