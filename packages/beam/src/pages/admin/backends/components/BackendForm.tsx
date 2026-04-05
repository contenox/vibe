import { Button, Form, FormField, Input, Section, Select, Spinner } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import { Backend } from '../../../../lib/types';

type BackendFormProps = {
  editingBackend: Backend | null;
  onCancel: () => void;
  onSubmit: (e: React.FormEvent) => void;
  isPending: boolean;
  error: boolean;
  name: string;
  setName: (value: string) => void;
  baseURL: string;
  setBaseURL: (value: string) => void;
  configType: string;
  setConfigType: (value: string) => void;
};

export default function BackendForm({
  editingBackend,
  onCancel,
  onSubmit,
  isPending,
  error,
  name,
  setName,
  baseURL,
  setBaseURL,
  configType,
  setConfigType,
}: BackendFormProps) {
  const { t } = useTranslation();

  const isFormValid = name.trim() && baseURL.trim() && configType.trim();

  return (
    <Section>
      <Form
        title={editingBackend ? t('backends.form_title_edit') : t('backends.form_title_create')}
        onSubmit={onSubmit}
        error={
          error
            ? t('errors.generic_upload', { action: editingBackend ? 'updating' : 'creating' })
            : undefined
        }
        actions={
          <div className="flex gap-2">
            <Button
              type="submit"
              variant="primary"
              disabled={isPending || !isFormValid}
              className="min-w-20">
              {isPending ? (
                <>
                  <Spinner size="sm" className="mr-2" />
                  {editingBackend ? t('common.updating') : t('common.creating')}
                </>
              ) : editingBackend ? (
                t('backends.form_update_action')
              ) : (
                t('backends.form_create_action')
              )}
            </Button>
            {editingBackend && (
              <Button type="button" variant="secondary" onClick={onCancel} disabled={isPending}>
                {t('common.cancel')}
              </Button>
            )}
          </div>
        }>
        <FormField label={t('common.name')} required>
          <Input
            value={name}
            onChange={e => setName(e.target.value)}
            placeholder="my-backend"
            disabled={isPending}
          />
        </FormField>

        <FormField label={t('backends.form_url')} required>
          <Input
            value={baseURL}
            onChange={e => setBaseURL(e.target.value)}
            placeholder="http://localhost:11434"
            disabled={isPending}
          />
        </FormField>

        <FormField label={t('backends.form_type')} required>
          <Select
            value={configType}
            onChange={e => setConfigType(e.target.value)}
            options={[
              { value: 'ollama', label: 'Ollama' },
              { value: 'vllm', label: 'vLLM' },
            ]}
            disabled={isPending}
          />
        </FormField>
      </Form>
    </Section>
  );
}
