// src/pages/admin/chains/components/TaskForm/TaskForm.tsx
import { Button, Panel, Section, Spinner, Tabs } from '@contenox/ui';
import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import type { ChainTask, FormTask } from '../../../../../lib/types';
import AdvancedFields from './fields/AdvancedFields';
import CommonFields from './fields/CommonFields';
import HandlerSpecificFields from './fields/HandlerSpecificFields';
import TransitionEditor from './fields/TransitionEditor';

interface TaskFormProps {
  task: ChainTask;
  onChange: (task: ChainTask) => void;
  onSave: () => void;
  onCancel: () => void;
  onDelete?: () => void;
  isSaving?: boolean;
  availableVariables?: string[];
}

type TabType = 'basic' | 'advanced' | 'transition';

export default function TaskForm({
  task,
  onChange,
  onSave,
  onCancel,
  onDelete,
  isSaving = false,
  availableVariables = ['input'],
}: TaskFormProps) {
  const { t } = useTranslation();
  const [activeTab, setActiveTab] = useState<TabType>('basic');
  const [formData, setFormData] = useState<FormTask>(() => ({
    ...task,
    transition: task.transition || { branches: [] },
    prompt_template: task.prompt_template || '',
  }));

  useEffect(() => {
    setFormData({
      ...task,
      transition: task.transition || { branches: [] },
      prompt_template: task.prompt_template || '',
    });
  }, [task]);

  const handleFieldChange = useCallback(
    (updates: Partial<FormTask>) => {
      const newFormData = { ...formData, ...updates };
      setFormData(newFormData);

      // Ensure hook has required tool_name if present
      const hook = newFormData.hook
        ? {
            ...newFormData.hook,
            tool_name: newFormData.hook.tool_name || '', // Required by backend
          }
        : undefined;

      const chainTask: ChainTask = {
        id: newFormData.id!,
        description: newFormData.description || '',
        handler: newFormData.handler!,
        prompt_template: newFormData.prompt_template!,
        transition: newFormData.transition!,
        system_instruction: newFormData.system_instruction,
        execute_config: newFormData.execute_config,
        hook,
        print: newFormData.print,
        output_template: newFormData.output_template,
        input_var: newFormData.input_var,
        timeout: newFormData.timeout,
        retry_on_failure: newFormData.retry_on_failure,
      };
      onChange(chainTask);
    },
    [formData, onChange],
  );

  const handleSave = useCallback(() => {
    onSave?.();
  }, [onSave]);

  const handleDelete = useCallback(() => {
    if (window.confirm(t('chains.confirm_delete_task'))) {
      onDelete?.();
    }
  }, [onDelete, t]);

  const tabs = [
    { id: 'basic' as const, label: t('chains.task_form.basic') },
    { id: 'advanced' as const, label: t('chains.task_form.advanced') },
    { id: 'transition' as const, label: t('chains.task_form.transition') },
  ];

  return (
    <Panel className="flex h-full flex-col">
      <Section
        title={formData.id || t('chains.task_form.new_task')}
        description={t('chains.task_form.configure_task')}
        className="shrink-0 border-b p-6">
        <Tabs tabs={tabs} activeTab={activeTab} onTabChange={id => setActiveTab(id as TabType)} />
      </Section>

      <div className="flex-1 overflow-y-auto p-6">
        {activeTab === 'basic' && (
          <div className="space-y-6">
            <CommonFields
              task={formData}
              onChange={handleFieldChange}
              availableVariables={availableVariables}
            />
            <HandlerSpecificFields task={formData} onChange={handleFieldChange} />
          </div>
        )}

        {activeTab === 'advanced' && (
          <div className="space-y-6">
            <AdvancedFields task={formData} onChange={handleFieldChange} />
          </div>
        )}

        {activeTab === 'transition' && (
          <TransitionEditor
            transition={formData.transition || { branches: [] }}
            onChange={transition => handleFieldChange({ transition })}
            availableVariables={availableVariables}
          />
        )}
      </div>

      <div className="bg-background/50 border-t p-6">
        <div className="flex items-center justify-between">
          <div>
            {onDelete && (
              <Button
                variant="danger"
                onClick={handleDelete}>
                {t('common.delete')}
              </Button>
            )}
          </div>
          <div className="flex gap-3">
            <Button variant="secondary" onClick={onCancel} disabled={isSaving} className="min-w-20">
              {t('common.cancel')}
            </Button>
            <Button variant="primary" onClick={handleSave} disabled={isSaving} className="min-w-20">
              {isSaving ? (
                <>
                  <Spinner size="sm" className="mr-2" />
                  {t('common.saving')}
                </>
              ) : (
                t('common.save')
              )}
            </Button>
          </div>
        </div>
      </div>
    </Panel>
  );
}
