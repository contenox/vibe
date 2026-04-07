import { Button, Form, FormField, Input, Select } from '@contenox/ui';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import type { Workspace } from '../../../../lib/types';

const SHELL_OPTIONS = [
  { value: '', label: 'Default' },
  { value: '/bin/bash', label: '/bin/bash' },
  { value: '/usr/bin/bash', label: '/usr/bin/bash' },
  { value: '/bin/zsh', label: '/bin/zsh' },
  { value: '/usr/bin/zsh', label: '/usr/bin/zsh' },
  { value: '/bin/sh', label: '/bin/sh' },
  { value: '/bin/dash', label: '/bin/dash' },
];

type Props = {
  editingWorkspace: Workspace | null;
  onCancel: () => void;
  onSubmit: (data: { name: string; path: string; shell?: string }) => void;
  isPending: boolean;
  error?: string;
};

export default function WorkspaceForm({
  editingWorkspace,
  onCancel,
  onSubmit,
  isPending,
  error,
}: Props) {
  const { t } = useTranslation();
  const [name, setName] = useState('');
  const [path, setPath] = useState('');
  const [shell, setShell] = useState('');

  useEffect(() => {
    if (editingWorkspace) {
      setName(editingWorkspace.name);
      setPath(editingWorkspace.path);
      setShell(editingWorkspace.shell ?? '');
    } else {
      setName('');
      setPath('');
      setShell('');
    }
  }, [editingWorkspace]);

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const trimmedName = name.trim();
    const trimmedPath = path.trim();
    if (!trimmedName || !trimmedPath) return;
    onSubmit({
      name: trimmedName,
      path: trimmedPath,
      shell: shell || undefined,
    });
  };

  return (
    <Form
      title={editingWorkspace ? t('workspaces.edit_title', 'Edit Workspace') : t('workspaces.create_title', 'Create Workspace')}
      onSubmit={handleSubmit}
      variant="surface"
      error={error}
      actions={
        <div className="flex gap-2">
          <Button type="submit" variant="primary" disabled={isPending || !name.trim() || !path.trim()}>
            {editingWorkspace ? t('common.save', 'Save') : t('common.create', 'Create')}
          </Button>
          {editingWorkspace && (
            <Button type="button" variant="secondary" onClick={onCancel} disabled={isPending}>
              {t('common.cancel', 'Cancel')}
            </Button>
          )}
        </div>
      }
    >
      <FormField label={t('workspaces.name_label', 'Name')} required>
        <Input
          value={name}
          onChange={e => setName(e.target.value)}
          placeholder={t('workspaces.name_placeholder', 'my-project')}
        />
      </FormField>
      <FormField label={t('workspaces.path_label', 'Path')} required>
        <Input
          value={path}
          onChange={e => setPath(e.target.value)}
          placeholder={t('workspaces.path_placeholder', '/home/user/projects/my-project')}
          className="font-mono"
        />
      </FormField>
      <FormField label={t('workspaces.shell_label', 'Shell')}>
        <Select
          options={SHELL_OPTIONS}
          value={shell}
          onChange={e => setShell(e.target.value)}
        />
      </FormField>
    </Form>
  );
}
