import { ErrorState, GridLayout, LoadingState, Panel, Section, Span } from '@contenox/ui';
import { t } from 'i18next';
import { useState } from 'react';
import {
  useCreateRemoteHook,
  useDeleteRemoteHook,
  useRemoteHooks,
  useUpdateRemoteHook,
} from '../../../../hooks/useRemoteHooks';
import { InjectionArg, RemoteHook } from '../../../../lib/types';
import RemoteHookCard from './RemoteHookCard';
import RemoteHookForm from './RemoteHookForm';

export default function RemoteHooksSection() {
  const [editingHook, setEditingHook] = useState<RemoteHook | null>(null);
  const [name, setName] = useState('');
  const [endpointUrl, setEndpointUrl] = useState('');
  const [timeoutMs, setTimeoutMs] = useState(5000);
  const [headers, setHeaders] = useState<Record<string, string>>({});
  const [properties, setProperties] = useState<InjectionArg>({
    name: '',
    value: '',
    in: 'body',
  });

  const { data: hooks, isLoading, error, refetch } = useRemoteHooks();
  const createMutation = useCreateRemoteHook();
  const updateMutation = useUpdateRemoteHook();
  const deleteMutation = useDeleteRemoteHook();

  const resetForm = () => {
    setName('');
    setEndpointUrl('');
    setTimeoutMs(5000);
    setHeaders({});
    setProperties({ name: '', value: '', in: 'body' });
    setEditingHook(null);
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();

    if (!name.trim() || !endpointUrl.trim()) {
      return;
    }

    const hookData: Partial<RemoteHook> = {
      name: name.trim(),
      endpointUrl: endpointUrl.trim(),
      timeoutMs,
      headers: Object.keys(headers).length > 0 ? headers : undefined,
      properties,
    };

    if (editingHook) {
      updateMutation.mutate({ id: editingHook.id, data: hookData }, { onSuccess: resetForm });
    } else {
      createMutation.mutate(hookData, { onSuccess: resetForm });
    }
  };

  const handleEdit = (hook: RemoteHook) => {
    setEditingHook(hook);
    setName(hook.name);
    setEndpointUrl(hook.endpointUrl);
    setTimeoutMs(hook.timeoutMs);
    setHeaders(hook.headers || {});
    setProperties(hook.properties);
  };

  const handleDelete = async (id: string) => {
    if (window.confirm(t('remote_hooks.delete_confirm'))) {
      await deleteMutation.mutateAsync(id);
    }
  };

  if (isLoading) {
    return <LoadingState message={t('remote_hooks.list_loading')} />;
  }

  if (error) {
    return <ErrorState error={error} onRetry={refetch} title={t('remote_hooks.list_error')} />;
  }

  return (
    <GridLayout variant="body" columns={2} responsive={{ base: 1, lg: 2 }} className="gap-6">
      {/* Left Column - Hooks List */}
      <Section className="space-y-4">
        <div className="flex items-center justify-between">
          <h2 className="text-lg font-semibold">{t('remote_hooks.manage_title')}</h2>
          <span className="text-muted text-sm">
            {t('remote_hooks.count', { count: hooks?.length || 0 })}
          </span>
        </div>

        <div className="max-h-[600px] space-y-4 overflow-y-auto">
          {hooks && hooks.length > 0 ? (
            hooks.map(hook => (
              <RemoteHookCard
                key={hook.id}
                hook={hook}
                onEdit={handleEdit}
                onDelete={handleDelete}
                isDeleting={deleteMutation.isPending}
              />
            ))
          ) : (
            <Panel variant="bordered" className="py-12 text-center">
              <div className="text-muted-foreground">
                <Span className="mb-2 block">{t('remote_hooks.list_empty_title')}</Span>
                <Span variant="muted" className="text-sm">
                  {t('remote_hooks.list_empty_description')}
                </Span>
              </div>
            </Panel>
          )}
        </div>
      </Section>

      {/* Right Column - Form */}
      <Section>
        <RemoteHookForm
          editingHook={editingHook}
          onCancel={resetForm}
          onSubmit={handleSubmit}
          isPending={editingHook ? updateMutation.isPending : createMutation.isPending}
          error={createMutation.isError || updateMutation.isError}
          name={name}
          setName={setName}
          endpointUrl={endpointUrl}
          setEndpointUrl={setEndpointUrl}
          timeoutMs={timeoutMs}
          setTimeoutMs={setTimeoutMs}
          headers={headers}
          setHeaders={setHeaders}
          properties={properties}
          setProperties={setProperties}
        />
      </Section>
    </GridLayout>
  );
}
