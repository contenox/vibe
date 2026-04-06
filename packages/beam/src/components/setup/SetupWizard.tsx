import { SetupWizardFlow } from './SetupWizardFlow';

/** Layout banner: shown only when setup issues exist and the user has not dismissed this signature. */
export function SetupWizard() {
  return <SetupWizardFlow variant="banner" />;
}
