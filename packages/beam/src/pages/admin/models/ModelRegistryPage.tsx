import {
  Button,
  EmptyState,
  ErrorState,
  FormField,
  GridLayout,
  Input,
  LoadingState,
  Page,
  Section,
} from '@contenox/ui';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  useCreateModelRegistryEntry,
  useDeleteModelRegistryEntry,
  useModelRegistry,
} from '../../../hooks/useModelRegistry';
import { ModelDescriptor } from '../../../lib/types';

function RegistryEntryRow({
  entry,
  onDelete,
}: {
  entry: ModelDescriptor;
  onDelete: (id: string) => void;
}) {
  const { t } = useTranslation();
  const sizeMB = entry.sizeBytes ? Math.round(entry.sizeBytes / 1024 / 1024) : 0;
  return (
    <div className="flex items-center justify-between rounded border p-3 text-sm">
      <div className="min-w-0 flex-1 space-y-0.5">
        <div className="flex items-center gap-2 font-medium">
          <span>{entry.name}</span>
          {entry.curated && (
            <span className="rounded bg-blue-100 px-1.5 py-0.5 text-xs text-blue-700">
              {t('model_registry.curated')}
            </span>
          )}
        </div>
        <div className="truncate text-xs text-gray-500">{entry.sourceUrl}</div>
        {sizeMB > 0 && (
          <div className="text-xs text-gray-400">{sizeMB} MB</div>
        )}
      </div>
      {!entry.curated && entry.id && (
        <Button variant="ghost" size="sm" onClick={() => onDelete(entry.id!)}>
          {t('common.delete')}
        </Button>
      )}
    </div>
  );
}

export default function ModelRegistryPage() {
  const { t } = useTranslation();
  const { data: entries, isLoading, error, refetch } = useModelRegistry();
  const createMutation = useCreateModelRegistryEntry();
  const deleteMutation = useDeleteModelRegistryEntry();

  const [name, setName] = useState('');
  const [sourceUrl, setSourceUrl] = useState('');

  const resetForm = () => {
    setName('');
    setSourceUrl('');
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    createMutation.mutate({ name, sourceUrl, sizeBytes: 0 }, { onSuccess: resetForm });
  };

  const handleDelete = (id: string) => {
    deleteMutation.mutate(id);
  };

  if (isLoading) {
    return <LoadingState message={t('model_registry.loading')} />;
  }

  if (error) {
    return <ErrorState error={error} onRetry={refetch} title={t('model_registry.list_error')} />;
  }

  const sorted = [...(entries ?? [])].sort((a, b) => {
    if (a.curated !== b.curated) return a.curated ? -1 : 1;
    return a.name.localeCompare(b.name);
  });

  return (
    <Page bodyScroll="auto" className="h-full">
      <GridLayout variant="body" columns={2} responsive={{ base: 1, lg: 2 }} className="gap-6 p-4">
        <Section title={t('model_registry.list_title')}>
          <div className="space-y-2">
            {sorted.length === 0 ? (
              <EmptyState
                title={t('model_registry.empty_title')}
                description={t('model_registry.empty_description')}
              />
            ) : (
              sorted.map(entry => (
                <RegistryEntryRow
                  key={entry.name}
                  entry={entry}
                  onDelete={handleDelete}
                />
              ))
            )}
          </div>
        </Section>

        <Section title={t('model_registry.add_title')}>
          <form onSubmit={handleSubmit} className="space-y-4">
            <FormField label={t('model_registry.form_name')} required>
              <Input
                value={name}
                onChange={e => setName(e.target.value)}
                placeholder="my-model"
                required
              />
            </FormField>
            <FormField label={t('model_registry.form_url')} required>
              <Input
                value={sourceUrl}
                onChange={e => setSourceUrl(e.target.value)}
                placeholder="https://huggingface.co/org/model.gguf"
                required
              />
            </FormField>
            <Button type="submit" disabled={createMutation.isPending}>
              {t('model_registry.form_add_action')}
            </Button>
          </form>
        </Section>
      </GridLayout>
    </Page>
  );
}
