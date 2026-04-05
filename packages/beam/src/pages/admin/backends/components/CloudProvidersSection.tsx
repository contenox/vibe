import { GridLayout, Section } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import ProviderForm from './ProviderForm';

export default function CloudProvidersSection() {
  const { t } = useTranslation();

  return (
    <GridLayout variant="body">
      <Section title={t('cloud_providers.ollama.title')}>
        <ProviderForm provider="ollama" />
      </Section>

      <Section title={t('cloud_providers.openai.title')}>
        <ProviderForm provider="openai" />
      </Section>

      <Section title={t('cloud_providers.gemini.title')}>
        <ProviderForm provider="gemini" />
      </Section>
    </GridLayout>
  );
}
