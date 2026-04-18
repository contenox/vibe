import { Button, Form, FormField, Input, Panel, Textarea } from '@contenox/ui';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useCreateBackend } from '../../../../hooks/useBackends';
import { useConfigureProvider, useProviderStatus } from '../../../../hooks/useProviders';
import type { CloudProviderType } from '../../../../lib/types';

type VertexProviderFormProps = {
  provider: CloudProviderType;
};

export default function VertexProviderForm({ provider }: VertexProviderFormProps) {
  const { t } = useTranslation();
  const [name, setName] = useState(provider);
  const [url, setUrl] = useState('');
  const [saJson, setSaJson] = useState('');
  const { data: status, isLoading } = useProviderStatus(provider);
  const configureMutation = useConfigureProvider(provider);
  const createBackend = useCreateBackend();

  const isPending = configureMutation.isPending || createBackend.isPending;
  const error = configureMutation.error ?? createBackend.error;

  const handleSubmit = () => {
    configureMutation.mutate(
      { apiKey: saJson, upsert: true },
      {
        onSuccess: () => {
          createBackend.mutate({ name, type: provider, baseUrl: url });
        },
      }
    );
  };

  return (
    <Form
      onSubmit={handleSubmit}
      actions={
        <Button type="submit" variant="primary" disabled={isPending || !url || !saJson}>
          {isPending ? t('common.configuring') : t('cloud_providers.configure_button')}
        </Button>
      }>
      <Panel variant="flat">{t('cloud_providers.vertex_adc_note')}</Panel>

      {error && <Panel variant="error">{error.message}</Panel>}
      {createBackend.isSuccess && (
        <Panel variant="flat">{t('cloud_providers.status_configured')}</Panel>
      )}
      {isLoading && <Panel variant="body">{t('common.loading')}</Panel>}
      {status?.configured && !createBackend.isSuccess && (
        <Panel variant="flat">{t('cloud_providers.status_configured')}</Panel>
      )}

      <FormField label={t('cloud_providers.backend_name')} required>
        <Input
          type="text"
          value={name}
          onChange={e => setName(e.target.value)}
          placeholder={provider}
        />
      </FormField>

      <FormField label={t('cloud_providers.vertex_url')} required>
        <Input
          type="text"
          value={url}
          onChange={e => setUrl(e.target.value)}
          placeholder="https://us-central1-aiplatform.googleapis.com/v1/projects/MY_PROJECT/locations/us-central1"
        />
      </FormField>

      <FormField label={t('cloud_providers.service_account_json')} required>
        <Textarea
          value={saJson}
          onChange={e => setSaJson(e.target.value)}
          placeholder={t('cloud_providers.service_account_json_placeholder')}
          rows={6}
        />
      </FormField>
    </Form>
  );
}
