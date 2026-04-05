// src/pages/admin/chains/components/TaskForm/fields/HookFields.tsx
import { Button, Input, Label, P, Textarea } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import { FormTask, HookCall } from '../../../../../../lib/types';

interface HookFieldsProps {
  task: FormTask;
  onChange: (updates: Partial<FormTask>) => void;
}

export default function HookFields({ task, onChange }: HookFieldsProps) {
  const { t } = useTranslation();

  const hook: HookCall = task.hook || { name: '', tool_name: '', args: {} };

  const updateHook = (updates: Partial<HookCall>) => {
    onChange({
      hook: {
        ...hook,
        ...updates,
      },
    });
  };

  const updateArg = (key: string, value: string) => {
    const next = { ...hook.args };
    if (value === '') {
      delete next[key];
    } else {
      next[key] = value;
    }
    updateHook({ args: next });
  };

  const addArg = () => {
    updateHook({ args: { ...hook.args, '': '' } });
  };

  const handleNameChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    updateHook({ name: e.target.value });
  };

  const handleToolNameChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    updateHook({ tool_name: e.target.value });
  };

  const handleOutputTemplateChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    onChange({ output_template: e.target.value });
  };

  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-4">
        <div>
          <Label className="block text-sm font-medium">{t('chains.task_form.hook_name')}</Label>
          <Input
            value={hook.name}
            onChange={handleNameChange}
            placeholder="slack_notification"
            required
          />
          <P className="text-text-muted mt-1 text-xs">{t('chains.task_form.hook_name_help')}</P>
        </div>

        <div>
          <Label className="block text-sm font-medium">{t('chains.task_form.tool_name')}</Label>
          <Input
            value={hook.tool_name}
            onChange={handleToolNameChange}
            placeholder="send_message"
            required
          />
          <P className="text-text-muted mt-1 text-xs">{t('chains.task_form.tool_name_help')}</P>
        </div>
      </div>

      <div>
        <Label className="block text-sm font-medium">{t('chains.task_form.hook_arguments')}</Label>
        <div className="space-y-2">
          {Object.entries(hook.args || {}).map(([key, val], index) => (
            <div key={index} className="flex gap-2">
              <Input
                value={key}
                onChange={(e: React.ChangeEvent<HTMLInputElement>) => {
                  const newKey = e.target.value;
                  const newArgs = { ...hook.args };
                  delete newArgs[key];
                  newArgs[newKey] = val;
                  updateHook({ args: newArgs });
                }}
                placeholder="argument_name"
                className="flex-1"
              />
              <Input
                value={val}
                onChange={(e: React.ChangeEvent<HTMLInputElement>) =>
                  updateArg(key, e.target.value)
                }
                placeholder="argument_value"
                className="flex-1"
              />
              <Button
                type="button"
                onClick={() => updateArg(key, '')}
                className="text-error hover:bg-error rounded-md px-3 py-2 transition-colors hover:text-white">
                ×
              </Button>
            </div>
          ))}
          <Button
            type="button"
            onClick={addArg}
            className="text-primary hover:text-primary-dark text-sm">
            {t('chains.task_form.add_argument')}
          </Button>
        </div>
        <P className="text-text-muted mt-1 text-xs">{t('chains.task_form.hook_arguments_help')}</P>
      </div>

      <div>
        <Label className="block text-sm font-medium">{t('chains.task_form.output_template')}</Label>
        <Textarea
          rows={3}
          value={task.output_template || ''}
          onChange={handleOutputTemplateChange}
          placeholder="Result: {{.status}} - {{.message}}"
        />
        <P className="text-text-muted mt-1 text-xs">{t('chains.task_form.output_template_help')}</P>
      </div>
    </div>
  );
}
