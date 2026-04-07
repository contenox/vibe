import { H1, P, Page } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import { SetupWizardFlow } from '../../../components/setup/SetupWizardFlow';

export default function SettingsPage() {
  const { t } = useTranslation();

  return (
    <Page bodyScroll="auto">
      <div className="mx-auto flex w-full max-w-4xl flex-col gap-6 p-4 md:p-6">
        <div className="space-y-1">
          <H1 variant="page">{t('settings.page_title')}</H1>
          <P variant="muted" className="max-w-2xl text-sm">
            {t('settings.page_description')}
          </P>
        </div>
        <SetupWizardFlow variant="page" />
      </div>
    </Page>
  );
}
