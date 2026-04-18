import { H2, P, Panel, Section } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import BackendsSection from '../../../pages/admin/backends/components/BackendsSection';
import ModelRegistryPage from '../../../pages/admin/models/ModelRegistryPage';
import ProviderForm from '../../../pages/admin/backends/components/ProviderForm';
import VertexProviderForm from '../../../pages/admin/backends/components/VertexProviderForm';
import type { ProviderChoice } from './StepChooseProvider';

type Props = {
  provider: ProviderChoice;
  onRefreshStatus: () => void;
};

export default function StepGetModel({ provider, onRefreshStatus: _onRefreshStatus }: Props) {
  const { t } = useTranslation();

  const desc =
    provider === 'local'
      ? t('onboarding.step_get_model.desc_local')
      : provider === 'ollama'
        ? t('onboarding.step_get_model.desc_ollama')
        : t('onboarding.step_get_model.desc_cloud');

  return (
    <div className="max-w-3xl mx-auto space-y-6">
      <div className="space-y-1">
        <H2 className="text-xl font-semibold">{t('onboarding.step_get_model.title')}</H2>
        <P variant="muted" className="text-sm">
          {desc}
        </P>
        {provider === 'ollama' && (
          <Panel variant="flat" className="text-xs mt-2">
            {t('onboarding.step_get_model.hint_ollama')}
          </Panel>
        )}
      </div>

      {provider === 'local' && <ModelRegistryPage />}

      {provider === 'ollama' && <BackendsSection />}

      {provider === 'openai' && (
        <Section title="OpenAI">
          <ProviderForm provider="openai" />
        </Section>
      )}

      {provider === 'gemini' && (
        <Section title="Gemini">
          <ProviderForm provider="gemini" />
        </Section>
      )}

      {provider === 'vertex' && (
        <div className="space-y-4">
          <Section title="Vertex AI — Google">
            <VertexProviderForm provider="vertex-google" />
          </Section>
          <Section title="Vertex AI — Anthropic">
            <VertexProviderForm provider="vertex-anthropic" />
          </Section>
          <Section title="Vertex AI — Meta">
            <VertexProviderForm provider="vertex-meta" />
          </Section>
          <Section title="Vertex AI — Mistral AI">
            <VertexProviderForm provider="vertex-mistralai" />
          </Section>
        </div>
      )}
    </div>
  );
}
