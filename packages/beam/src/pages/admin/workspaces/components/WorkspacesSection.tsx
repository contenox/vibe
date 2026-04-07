import {
  ErrorState,
  GridLayout,
  LoadingState,
  P,
  Panel,
  ResourceCard,
  Section,
  Span,
} from '@contenox/ui';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  useCreateWorkspace,
  useDeleteWorkspace,
  useUpdateWorkspace,
  useWorkspaces,
} from '../../../../hooks/useWorkspaces';
import type { Workspace } from '../../../../lib/types';
import WorkspaceForm from './WorkspaceForm';

export default function WorkspacesSection() {
  const { t } = useTranslation();
  const { data: workspaces, isLoading, error, refetch } = useWorkspaces();
  const createMutation = useCreateWorkspace();
  const updateMutation = useUpdateWorkspace();
  const deleteMutation = useDeleteWorkspace();
  const [editing, setEditing] = useState<Workspace | null>(null);

  const handleSubmit = (data: { name: string; path: string; shell?: string }) => {
    if (editing) {
      updateMutation.mutate(
        { id: editing.id, body: data },
        { onSuccess: () => setEditing(null) },
      );
    } else {
      createMutation.mutate(data);
    }
  };

  const handleEdit = (ws: Workspace) => setEditing(ws);
  const handleDelete = (ws: Workspace) => {
    if (window.confirm(t('workspaces.delete_confirm', { name: ws.name }))) {
      deleteMutation.mutate(ws.id);
    }
  };
  const resetForm = () => setEditing(null);

  if (isLoading) return <LoadingState message={t('workspaces.loading', 'Loading workspaces...')} />;
  if (error) return <ErrorState error={error} onRetry={refetch} title={t('workspaces.list_error', 'Failed to load workspaces')} />;

  return (
    <GridLayout variant="body" columns={2} responsive={{ base: 1, lg: 2 }} className="gap-6">
      <div className="space-y-6">
        <Section title={t('workspaces.manage_title', 'Workspaces')}>
          <Span variant="muted" className="text-sm">
            {t('workspaces.count', { count: workspaces?.length ?? 0 })}
          </Span>
          <div className="mt-4 space-y-4">
            {workspaces && workspaces.length > 0 ? (
              workspaces.map(ws => (
                <ResourceCard
                  key={ws.id}
                  title={ws.name}
                  subtitle={ws.path}
                  actions={{
                    edit: () => handleEdit(ws),
                    delete: () => handleDelete(ws),
                  }}
                >
                  <div className="flex flex-wrap gap-x-6 gap-y-1 text-xs">
                    <Span variant="muted">
                      {t('workspaces.shell_display', 'Shell')}: <Span className="font-mono">{ws.shell || t('workspaces.default_shell', 'default')}</Span>
                    </Span>
                    {ws.vfsPath && (
                      <Span variant="muted">
                        VFS: <Span className="font-mono">{ws.vfsPath}</Span>
                      </Span>
                    )}
                  </div>
                </ResourceCard>
              ))
            ) : (
              <Panel variant="bordered" className="py-12 text-center">
                <P variant="muted" className="mb-2">
                  {t('workspaces.empty_title', 'No workspaces')}
                </P>
                <P variant="muted" className="text-sm">
                  {t('workspaces.empty_description', 'Create a workspace to define where terminals and file operations run.')}
                </P>
              </Panel>
            )}
          </div>
        </Section>
      </div>

      <div className="space-y-6">
        <WorkspaceForm
          editingWorkspace={editing}
          onCancel={resetForm}
          onSubmit={handleSubmit}
          isPending={editing ? updateMutation.isPending : createMutation.isPending}
          error={
            (editing ? updateMutation.error?.message : createMutation.error?.message) || undefined
          }
        />
      </div>
    </GridLayout>
  );
}
