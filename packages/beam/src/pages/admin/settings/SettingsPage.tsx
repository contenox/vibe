import { P } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import { Page } from '../../../components/Page';
import { SetupWizardFlow } from '../../../components/setup/SetupWizardFlow';

export default function SettingsPage() {
  const { t } = useTranslation();

  return (
    <Page bodyScroll="auto">
      <div className="mx-auto flex w-full max-w-4xl flex-col gap-6 p-4 md:p-6">
        <div className="space-y-1">
          <h1 className="text-text dark:text-dark-text text-2xl font-semibold">
            {t('settings.page_title')}
          </h1>
          <P variant="muted" className="max-w-2xl text-sm">
            {t('settings.page_description')}
          </P>
        </div>
        <SetupWizardFlow variant="page" />
      </div>
    </Page>
  );
}
