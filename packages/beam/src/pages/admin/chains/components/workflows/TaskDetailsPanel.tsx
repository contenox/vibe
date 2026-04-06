import { DetailsPanel, Panel } from '@contenox/ui';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { CHAIN_HANDLER_OPTIONS } from '../../../../../lib/chainHandlerOptions';
import { ChainTask } from '../../../../../lib/types';

interface TaskDetailsPanelProps {
  task: ChainTask;
  onClose: () => void;
  onSave: (task: ChainTask) => void;
  onDelete?: (taskId: string) => void;
  isNewTask?: boolean;
}

interface ExecuteConfigDisplay {
  provider?: string;
  model?: string;
  temperature?: number;
  max_tokens?: number;
}

interface HookConfigDisplay {
  name?: string;
  args?: Record<string, unknown>;
}

const TaskDetailsPanel: React.FC<TaskDetailsPanelProps> = ({
  task,
  onClose,
  onSave,
  onDelete,
  isNewTask = false,
}) => {
  const { t } = useTranslation();
  const [isEditing, setIsEditing] = useState(isNewTask);
  const [editedTask, setEditedTask] = useState<ChainTask>({ ...task });

  // Update edited task when task prop changes
  useEffect(() => {
    setEditedTask({ ...task });
  }, [task]);

  const handlerOptions = CHAIN_HANDLER_OPTIONS.map(option => ({
    value: option.value,
    label: option.label,
  }));

  const fields = [
    {
      key: 'id',
      label: t('chains.task_id'),
      type: 'text' as const,
    },
    {
      key: 'handler',
      label: t('workflow.task_type'),
      type: 'select' as const,
      options: handlerOptions,
    },
    {
      key: 'description',
      label: t('workflow.description'),
      type: 'text' as const,
    },
    {
      key: 'prompt_template',
      label: t('workflow.prompt_template'),
      type: 'textarea' as const,
    },
    {
      key: 'input_var',
      label: t('workflow.input_variable'),
      type: 'text' as const,
    },
    {
      key: 'system_instruction',
      label: t('chains.system_instruction'),
      type: 'textarea' as const,
    },
    {
      key: 'timeout',
      label: t('workflow.timeout'),
      type: 'text' as const,
    },
    {
      key: 'retry_on_failure',
      label: t('workflow.retry_on_failure'),
      type: 'text' as const,
    },
  ];

  // Custom render for complex nested data
  const renderTransitionData = (transition: ChainTask['transition']) => {
    return (
      <div className="space-y-3">
        <div>
          <strong>{t('workflow.on_failure')}:</strong> {transition.on_failure || 'None'}
        </div>
        <div>
          <strong>{t('workflow.branches')}:</strong>
          <div className="mt-2 space-y-2">
            {transition.branches.map((branch, index) => (
              <Panel key={index} variant="body" className="pl-3">
                <div>
                  <strong>Condition:</strong> {branch.when || 'Default'}
                </div>
                <div>
                  <strong>Go to:</strong> {branch.goto}
                </div>
                <div>
                  <strong>Operator:</strong> {branch.operator}
                </div>
                {branch.compose && (
                  <div className="mt-2 text-xs">
                    <div>
                      <strong>compose.with_var:</strong> {branch.compose.with_var || '(none)'}
                    </div>
                    <div>
                      <strong>compose.strategy:</strong> {branch.compose.strategy || '(default)'}
                    </div>
                  </div>
                )}
              </Panel>
            ))}
          </div>
        </div>
      </div>
    );
  };

  const renderExecuteConfig = (value: ExecuteConfigDisplay | null | undefined) => (
    <div className="space-y-2">
      {value?.provider && (
        <div>
          <strong>Provider:</strong> {value.provider}
        </div>
      )}
      {value?.model && (
        <div>
          <strong>Model:</strong> {value.model}
        </div>
      )}
      {value?.temperature && (
        <div>
          <strong>Temperature:</strong> {value.temperature}
        </div>
      )}
      {value?.max_tokens && (
        <div>
          <strong>Max Tokens:</strong> {value.max_tokens}
        </div>
      )}
    </div>
  );

  const renderHookConfig = (value: HookConfigDisplay | null | undefined) => (
    <div className="space-y-2">
      {value?.name && (
        <div>
          <strong>Hook Name:</strong> {value.name}
        </div>
      )}
      {value?.args && Object.keys(value.args).length > 0 && (
        <div>
          <strong>Arguments:</strong>
          <Panel variant="surface" className="m-0 mt-1 p-2">
            <pre className="text-text dark:text-dark-text overflow-auto font-mono text-xs">
              {JSON.stringify(value.args, null, 2)}
            </pre>
          </Panel>
        </div>
      )}
    </div>
  );

  const extendedFields = [
    ...fields,
    {
      key: 'transition',
      label: t('workflow.transitions'),
      type: 'custom' as const,
      render: (value: ChainTask['transition']) => renderTransitionData(value),
    },
    {
      key: 'execute_config',
      label: t('chains.execute_configuration'),
      type: 'custom' as const,
      render: (value: ExecuteConfigDisplay | null | undefined) => renderExecuteConfig(value),
    },
    {
      key: 'hook',
      label: t('chains.hook_configuration'),
      type: 'custom' as const,
      render: (value: HookConfigDisplay | null | undefined) => renderHookConfig(value),
    },
  ];

  const handleEditToggle = (editing: boolean) => {
    setIsEditing(editing);
    if (!editing && !isNewTask) {
      setEditedTask({ ...task });
    }
  };

  const handleFieldUpdate = (updates: Record<string, any>) => {
    setEditedTask(prev => ({
      ...prev,
      ...updates,
    }));
  };

  const handleSave = (data: Record<string, any>) => {
    // Convert the generic data back to ChainTask
    const updatedTask: ChainTask = {
      id: data.id || editedTask.id,
      description: data.description || editedTask.description,
      handler: data.handler || editedTask.handler,
      prompt_template: data.prompt_template || editedTask.prompt_template,
      transition: data.transition || editedTask.transition,
      system_instruction: data.system_instruction,
      valid_conditions: data.valid_conditions,
      execute_config: data.execute_config,
      hook: data.hook,
      print: data.print,
      output_template: data.output_template,
      input_var: data.input_var,
      timeout: data.timeout,
      retry_on_failure: data.retry_on_failure,
    };

    onSave(updatedTask);
    if (!isNewTask) {
      setIsEditing(false);
    }
  };

  return (
    <DetailsPanel
      title={editedTask.id}
      data={editedTask}
      fields={extendedFields}
      onClose={onClose}
      onSave={handleSave}
      onDelete={onDelete ? () => onDelete(editedTask.id) : undefined}
      isEditing={isEditing}
      onEditToggle={handleEditToggle}
      onFieldUpdate={handleFieldUpdate}
    />
  );
};

export default TaskDetailsPanel;
