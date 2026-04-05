import { Button, Card, FormField, Label, Panel, Spinner, Textarea } from '@contenox/ui';
import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useUpdateChain } from '../../../../hooks/useChains';
import { ChainDefinition, ChainTask } from '../../../../lib/types';

interface ChainEditorProps {
  chain: ChainDefinition;
  /** VFS path for PUT /api/taskchains?path= */
  vfsPath: string;
  selectedTask?: ChainTask | null;
  onTaskSelect?: (task: ChainTask) => void;
  highlightTaskId?: string | null;
  onHighlightReset?: () => void;
}

export default function ChainEditor({
  chain,
  vfsPath,
  selectedTask,
  onTaskSelect,
  highlightTaskId,
  onHighlightReset,
}: ChainEditorProps) {
  const { t } = useTranslation();
  const updateChain = useUpdateChain(vfsPath);
  const [tasks, setTasks] = useState(JSON.stringify(chain.tasks, null, 2));
  const [tasksError, setTasksError] = useState('');
  const textareaRef = useRef<HTMLTextAreaElement>(null);

  // Scroll to highlighted task
  useEffect(() => {
    if (highlightTaskId && textareaRef.current) {
      const taskIndex = chain.tasks.findIndex(t => t.id === highlightTaskId);
      if (taskIndex !== -1) {
        const lineHeight = 20; // Approximate line height
        const scrollPosition = taskIndex * lineHeight * 10; // 10 lines per task
        textareaRef.current.scrollTop = scrollPosition;
      }
    }
  }, [highlightTaskId, chain.tasks]);

  const handleSave = () => {
    try {
      const parsedTasks = JSON.parse(tasks);
      updateChain.mutate({
        ...chain,
        tasks: parsedTasks,
      });
      setTasksError('');
      onHighlightReset?.();
    } catch (err) {
      setTasksError(t('chains.invalid_json') + (err as Error).message);
    }
  };

  return (
    <Card variant="bordered" className="p-4">
      {updateChain.error && (
        <Panel variant="error" className="mb-4">
          {updateChain.error.message}
        </Panel>
      )}

      <div className="mb-4">
        <Label>{t('chains.form_id')}</Label>
        <div>{chain.id}</div>
      </div>

      <div className="mb-4">
        <Label>{t('chains.form_description')}</Label>
        <div>{chain.description}</div>
      </div>

      <FormField label={t('chains.form_tasks')} error={tasksError}>
        <Textarea
          ref={textareaRef}
          value={tasks}
          onChange={e => setTasks(e.target.value)}
          className="h-max min-h-[400px] font-mono text-sm"
        />

        {/* Task navigation bar */}
        <div className="mt-4">
          <Label>{t('workflow.task_navigation')}</Label>
          <div className="mt-2 flex flex-wrap gap-2">
            {chain.tasks.map(task => (
              <Button
                key={task.id}
                size="sm"
                variant={
                  highlightTaskId === task.id
                    ? 'primary'
                    : selectedTask?.id === task.id
                      ? 'accent'
                      : 'secondary'
                }
                onClick={() => {
                  onTaskSelect?.(task);
                  onHighlightReset?.();
                }}>
                {task.id}
              </Button>
            ))}
          </div>
        </div>
      </FormField>

      <div className="mt-4 flex justify-end">
        <Button variant="primary" onClick={handleSave} disabled={updateChain.isPending}>
          {updateChain.isPending ? <Spinner size="sm" /> : t('common.save')}
        </Button>
      </div>
    </Card>
  );
}
