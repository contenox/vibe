import { GridLayout, H2, P, Panel } from '@contenox/ui';
import { t } from 'i18next';
import { useState } from 'react';
import { ErrorState, LoadingState } from '../../../../components/LoadingState';
import {
  useBackends,
  useCreateBackend,
  useDeleteBackend,
  useUpdateBackend,
} from '../../../../hooks/useBackends';
import { Backend } from '../../../../lib/types';
import { BackendCard } from './BackendCard';
import BackendForm from './BackendForm';

export default function BackendsSection() {
  const { data: backends, isLoading, error, refetch } = useBackends();
  const createBackendMutation = useCreateBackend();
  const updateBackendMutation = useUpdateBackend();
  const deleteBackendMutation = useDeleteBackend();

  const [editingBackend, setEditingBackend] = useState<Backend | null>(null);
  const [name, setName] = useState('');
  const [baseURL, setBaseURL] = useState('');
  const [configType, setConfigType] = useState('ollama');

  const resetForm = () => {
    setName('');
    setBaseURL('');
    setConfigType('ollama');
    setEditingBackend(null);
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (editingBackend) {
      updateBackendMutation.mutate(
        { id: editingBackend.id, data: { name, baseUrl: baseURL, type: configType } },
        { onSuccess: resetForm },
      );
    } else {
      createBackendMutation.mutate(
        { name, baseUrl: baseURL, type: configType },
        { onSuccess: resetForm },
      );
    }
  };

  const handleEdit = (backend: Backend) => {
    setEditingBackend(backend);
    setName(backend.name);
    setBaseURL(backend.baseUrl);
    setConfigType(backend.type);
  };

  const handleDelete = async (id: string) => {
    await deleteBackendMutation.mutateAsync(id);
  };

  if (isLoading) {
    return <LoadingState message={t('backends.loading')} />;
  }

  if (error) {
    return <ErrorState error={error} onRetry={refetch} title={t('backends.list_error')} />;
  }

  return (
    <GridLayout variant="body" columns={2} responsive={{ base: 1, lg: 2 }} className="gap-6">
      <div className="space-y-6">
        <div className="bg-surface rounded-lg p-6">
          <H2 variant="sectionTitle" className="mb-4">
            {t('backends.manage_title')}
          </H2>

          <div className="space-y-4">
            {backends && backends.length > 0 ? (
              backends.map(backend => (
                <BackendCard
                  key={backend.id}
                  backend={backend}
                  onEdit={handleEdit}
                  onDelete={handleDelete}
                />
              ))
            ) : (
              <Panel variant="bordered" className="py-12 text-center">
                <div className="text-muted-foreground">
                  <P variant="muted" className="mb-2">
                    {t('backends.empty_title')}
                  </P>
                  <P variant="muted" className="text-sm">
                    {t('backends.empty_description')}
                  </P>
                </div>
              </Panel>
            )}
          </div>
        </div>
      </div>

      <div className="space-y-6">
        <BackendForm
          editingBackend={editingBackend}
          onCancel={resetForm}
          onSubmit={handleSubmit}
          isPending={
            editingBackend ? updateBackendMutation.isPending : createBackendMutation.isPending
          }
          error={createBackendMutation.isError || updateBackendMutation.isError}
          name={name}
          setName={setName}
          baseURL={baseURL}
          setBaseURL={setBaseURL}
          configType={configType}
          setConfigType={setConfigType}
        />
      </div>
    </GridLayout>
  );
}
