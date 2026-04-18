import { GridLayout, Section } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import ProviderForm from './ProviderForm';
import VertexProviderForm from './VertexProviderForm';

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

      <Section title={t('cloud_providers.vertex_google.title')}>
        <VertexProviderForm provider="vertex-google" />
      </Section>

      <Section title={t('cloud_providers.vertex_anthropic.title')}>
        <VertexProviderForm provider="vertex-anthropic" />
      </Section>

      <Section title={t('cloud_providers.vertex_meta.title')}>
        <VertexProviderForm provider="vertex-meta" />
      </Section>

      <Section title={t('cloud_providers.vertex_mistralai.title')}>
        <VertexProviderForm provider="vertex-mistralai" />
      </Section>
    </GridLayout>
  );
}
