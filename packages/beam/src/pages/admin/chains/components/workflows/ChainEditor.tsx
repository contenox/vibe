import { Button, Card, Checkbox, FormField, Label, Panel, Spinner, Textarea } from '@contenox/ui';
import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useUpdateChain } from '../../../../../hooks/useChains';
import { ChainDefinition, ChainTask } from '../../../../../lib/types';

interface ChainEditorProps {
  chain: ChainDefinition;
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

  useEffect(() => {
    if (highlightTaskId && textareaRef.current) {
      const taskIndex = chain.tasks.findIndex(t => t.id === highlightTaskId);
      if (taskIndex !== -1) {
        const lineHeight = 20;
        const scrollPosition = taskIndex * lineHeight * 10;
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
    <Card variant="bordered" className="flex h-full min-h-[640px] flex-col p-6">
      {updateChain.error && (
        <Panel variant="error" className="mb-4">
          {updateChain.error.message}
        </Panel>
      )}

      <div className="space-y-6">
        <div>
          <Label className="text-sm font-medium">{t('chains.form_id')}</Label>
          <div className="mt-1 font-mono text-sm">{chain.id}</div>
        </div>

        <div>
          <Label className="text-sm font-medium">{t('chains.form_description')}</Label>
          <div className="text-muted-foreground mt-1 text-sm">
            {chain.description || t('chains.no_description')}
          </div>
        </div>

        <div className="flex items-center gap-3">
          <Checkbox
            checked={!!chain.debug}
            onChange={(e: React.ChangeEvent<HTMLInputElement>) => {
              console.log('Debug mode:', e.target.checked);
            }}
          />
          <Label className="cursor-pointer text-sm font-medium">{t('chains.enable_debug')}</Label>
        </div>

        <FormField label={t('chains.form_tasks')} error={tasksError} className="min-h-0 flex-1">
          <Textarea
            ref={textareaRef}
            value={tasks}
            onChange={e => setTasks(e.target.value)}
            className="h-full max-h-[70vh] min-h-[420px] flex-1 resize-y font-mono text-sm whitespace-pre"
            rows={20}
          />

          {/* Task navigation bar */}
          <div className="mt-4">
            <Label className="text-sm font-medium">{t('workflow.task_navigation')}</Label>
            <div className="mt-3 flex flex-wrap gap-2">
              {chain.tasks.map((task: ChainTask) => (
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

        <div className="flex justify-end border-t pt-4">
          <Button
            variant="primary"
            onClick={handleSave}
            disabled={updateChain.isPending}
            className="min-w-24">
            {updateChain.isPending ? <Spinner size="sm" /> : t('common.save')}
          </Button>
        </div>
      </div>
    </Card>
  );
}
