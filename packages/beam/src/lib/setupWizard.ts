import type { SetupStatus } from './types';
import type { WizardStepStatus } from '@contenox/ui';

export type SetupWizardStepId = 'defaults' | 'register' | 'health';

export type SetupWizardDerivedStep = {
  id: SetupWizardStepId;
  status: WizardStepStatus;
  /** First incomplete step (for aria-current). */
  active: boolean;
};

function issueCodes(setup: SetupStatus): Set<string> {
  const issues = setup.issues ?? [];
  return new Set(issues.map(i => i.code));
}

function issuesForCategory(setup: SetupStatus, category: string) {
  const issues = setup.issues ?? [];
  return issues.filter(issue => issue.category === category);
}

/** 0-based index of the first step that still needs attention; 0 if unsure. */
export function getRecommendedSetupStepIndex(setup: SetupStatus): number {
  const steps = deriveSetupWizardSteps(setup);
  const i = steps.findIndex(s => s.active);
  return i >= 0 ? i : 0;
}

/**
 * Maps GET /setup-status payload to three fixed wizard steps (defaults → register backend → health).
 */
export function deriveSetupWizardSteps(setup: SetupStatus): SetupWizardDerivedStep[] {
  const codes = issueCodes(setup);
  const defaultIssues = issuesForCategory(setup, 'defaults');
  const registrationIssues = issuesForCategory(setup, 'registration');
  const healthIssues = issuesForCategory(setup, 'health');
  const registrationHasError = registrationIssues.some(issue => issue.severity === 'error');
  const healthHasError = healthIssues.some(issue => issue.severity === 'error');

  const defaultsDone = defaultIssues.length === 0;
  const registerDone = setup.backendCount > 0 && !registrationHasError && !codes.has('no_backends');
  const healthDone =
    setup.backendCount > 0 &&
    setup.reachableBackendCount > 0 &&
    !healthHasError;

  const step1Status: WizardStepStatus = defaultsDone ? 'complete' : 'error';
  let step2Status: WizardStepStatus;
  if (registerDone) {
    step2Status = 'complete';
  } else if (!defaultsDone) {
    step2Status = 'upcoming';
  } else if (registrationHasError) {
    step2Status = 'error';
  } else {
    step2Status = 'current';
  }
  let step3Status: WizardStepStatus;
  if (!registerDone) {
    step3Status = 'upcoming';
  } else if (healthDone) {
    step3Status = 'complete';
  } else if (healthHasError) {
    step3Status = 'error';
  } else {
    step3Status = 'current';
  }

  let activeIndex = 0;
  if (!defaultsDone) activeIndex = 0;
  else if (!registerDone) activeIndex = 1;
  else if (!healthDone) activeIndex = 2;
  else activeIndex = -1;

  return [
    { id: 'defaults', status: step1Status, active: activeIndex === 0 },
    { id: 'register', status: step2Status, active: activeIndex === 1 },
    { id: 'health', status: step3Status, active: activeIndex === 2 },
  ];
}
