import { describe, expect, it } from 'vitest';
import type { SetupStatus } from './types';
import { deriveSetupWizardSteps } from './setupWizard';

function status(partial: Partial<SetupStatus>): SetupStatus {
  return {
    defaultModel: '',
    defaultProvider: '',
    defaultChain: '',
    backendCount: 0,
    reachableBackendCount: 0,
    issues: [],
    backendChecks: [],
    ...partial,
  };
}

describe('deriveSetupWizardSteps', () => {
  it('marks defaults step error and active when model missing', () => {
    const s = status({
      issues: [{ code: 'missing_default_model', severity: 'error', category: 'defaults', message: 'm' }],
    });
    const steps = deriveSetupWizardSteps(s);
    expect(steps[0]).toMatchObject({ id: 'defaults', status: 'error', active: true });
    expect(steps[1].status).toBe('upcoming');
    expect(steps[2].status).toBe('upcoming');
  });

  it('activates register when defaults ok but no backends', () => {
    const s = status({
      defaultModel: 'm',
      defaultProvider: 'openai',
      issues: [{ code: 'no_backends', severity: 'warning', category: 'registration', message: 'n' }],
    });
    const steps = deriveSetupWizardSteps(s);
    expect(steps[0]).toMatchObject({ status: 'complete', active: false });
    expect(steps[1]).toMatchObject({ status: 'current', active: true });
    expect(steps[2].status).toBe('upcoming');
  });

  it('marks health error when all backends unreachable', () => {
    const s = status({
      defaultModel: 'm',
      defaultProvider: 'ollama',
      backendCount: 1,
      reachableBackendCount: 0,
      issues: [{ code: 'all_backends_unreachable', severity: 'error', category: 'health', message: 'x' }],
    });
    const steps = deriveSetupWizardSteps(s);
    expect(steps[0].status).toBe('complete');
    expect(steps[1].status).toBe('complete');
    expect(steps[2]).toMatchObject({ id: 'health', status: 'error', active: true });
  });

  it('marks health error for provider-specific health issues', () => {
    const s = status({
      defaultModel: 'gpt-5',
      defaultProvider: 'openai',
      backendCount: 2,
      reachableBackendCount: 1,
      issues: [
        {
          code: 'default_provider_auth_failed',
          severity: 'error',
          category: 'health',
          message: 'auth failed',
        },
      ],
    });
    const steps = deriveSetupWizardSteps(s);
    expect(steps[2]).toMatchObject({ id: 'health', status: 'error', active: true });
  });

  it('completes health when reachable', () => {
    const s = status({
      defaultModel: 'm',
      defaultProvider: 'ollama',
      backendCount: 2,
      reachableBackendCount: 2,
      issues: [],
    });
    const steps = deriveSetupWizardSteps(s);
    expect(steps.every(x => x.status === 'complete')).toBe(true);
    expect(steps.every(x => !x.active)).toBe(true);
  });
});
