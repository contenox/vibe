import { Button, Form, FormField, Input, Textarea } from '@contenox/ui';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useCreateChain, useUpdateChain } from '../../../../hooks/useChains';
import { ChainDefinition } from '../../../../lib/types';

interface ChainFormProps {
  /** VFS path for the JSON file (e.g. my-chain.json). */
  vfsPath: string;
  chain?: ChainDefinition;
}

export default function ChainForm({ vfsPath, chain }: ChainFormProps) {
  const { t } = useTranslation();
  const createChain = useCreateChain();
  const updateChain = useUpdateChain(vfsPath);

  const [id, setId] = useState(chain?.id || '');
  const [description, setDescription] = useState(chain?.description || '');
  const [tasks, setTasks] = useState(JSON.stringify(chain?.tasks || [], null, 2));
  const [tasksError, setTasksError] = useState('');

  const isEditing = !!chain;
  const isPending = isEditing ? updateChain.isPending : createChain.isPending;
  const error = isEditing ? updateChain.error : createChain.error;

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();

    let parsedTasks;
    try {
      parsedTasks = JSON.parse(tasks);
      setTasksError('');
    } catch (err) {
      setTasksError(t('chains.invalid_json') + err);
      return;
    }

    const chainData: ChainDefinition = {
      id,
      description,
      tasks: parsedTasks,
      token_limit: chain?.token_limit || 4096,
    };

    if (isEditing) {
      updateChain.mutate(chainData);
    } else {
      createChain.mutate({ vfsPath, chain: chainData });
    }
  };

  return (
    <Form
      title={isEditing ? t('chains.form_edit_title') : t('chains.form_create_title')}
      onSubmit={handleSubmit}
      error={error?.message}
      actions={
        <>
          <Button type="submit" variant="primary" disabled={isPending}>
            {isPending
              ? t('common.saving')
              : isEditing
                ? t('chains.form_update_action')
                : t('chains.form_create_action')}
          </Button>
          {isEditing && chain && (
            <Button
              type="button"
              variant="secondary"
              onClick={() => {
                setId(chain.id);
                setDescription(chain.description);
                setTasks(JSON.stringify(chain.tasks, null, 2));
              }}>
              {t('common.reset')}
            </Button>
          )}
        </>
      }>
      <FormField label={t('chains.form_id')} required>
        <Input value={id} onChange={e => setId(e.target.value)} disabled={isEditing} />
      </FormField>

      <FormField label={t('chains.form_description')}>
        <Input value={description} onChange={e => setDescription(e.target.value)} />
      </FormField>

      <FormField label={t('chains.form_tasks')} required error={tasksError}>
        <Textarea
          value={tasks}
          onChange={e => setTasks(e.target.value)}
          className="min-h-[300px] font-mono text-sm"
          placeholder={t('chains.tasks_placeholder')}
        />
      </FormField>
    </Form>
  );
}
