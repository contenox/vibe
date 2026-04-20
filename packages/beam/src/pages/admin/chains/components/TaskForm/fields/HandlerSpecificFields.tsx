import { Panel, Textarea, Label, P } from '@contenox/ui';
import { useTranslation } from 'react-i18next';
import {
  FormTask,
  HandleChatCompletion,
  HandleExecuteToolCalls,
  HandleHook,
  HandleNoop,
  HandleParseNumber,
  HandlePromptToString,
  HandleRaiseError,
} from '../../../../../../lib/types';
import HookFields from './HookFields';
import LLMConfigFields from './LLMConfigFields';

interface HandlerSpecificFieldsProps {
  task: FormTask;
  onChange: (updates: Partial<FormTask>) => void;
}

export default function HandlerSpecificFields({ task, onChange }: HandlerSpecificFieldsProps) {
  const { t } = useTranslation();

  const renderPromptBlock = (placeholder: string) => (
    <div>
      <Label className="block text-sm font-medium">{t('chains.task_form.prompt_template')}</Label>
      <Textarea
        rows={4}
        value={task.prompt_template || ''}
        onChange={(e: React.ChangeEvent<HTMLTextAreaElement>) =>
          onChange({ prompt_template: e.target.value })
        }
        placeholder={placeholder}
      />
      {/*{help ? <p className="text-text-muted mt-1 text-xs">{t(help)}</p> : null}*/}
    </div>
  );

  // hook
  if (task.handler === HandleHook) {
    return (
      <Panel variant="surface" className="p-4">
        <HookFields task={task} onChange={onChange} />
      </Panel>
    );
  }

  // LLM-like
  if (task.handler === HandleChatCompletion || task.handler === HandleExecuteToolCalls) {
    return (
      <Panel variant="surface" className="p-4">
        <LLMConfigFields task={task} onChange={onChange} expanded />
      </Panel>
    );
  }

  // parsing / raw
  if (task.handler === HandleParseNumber || task.handler === HandlePromptToString) {
    const placeholders: Record<string, string> = {
      [HandleParseNumber]: 'Extract the numeric value from: {{.input}}',
      [HandlePromptToString]: 'Enter your prompt template...',
    };

    // const helps: Record<string, string> = {
    //   [HandleParseNumber]: 'chains.task_form.prompt_template_number_help',
    //   [HandleParseScore]: 'chains.task_form.prompt_template_score_help',
    //   [HandleParseRange]: 'chains.task_form.prompt_template_range_help',
    //   [HandleParseKeyValue]: 'chains.task_form.prompt_template_keyvalue_help',
    //   [HandleRawString]: 'chains.task_form.prompt_template_default_help',
    // };

    return (
      <Panel variant="surface" className="space-y-4 p-4">
        {renderPromptBlock(placeholders[task.handler])}
        <LLMConfigFields task={task} onChange={onChange} />
      </Panel>
    );
  }

  // raise_error
  if (task.handler === HandleRaiseError) {
    return (
      <Panel variant="surface" className="space-y-2 p-4">
        <Label className="block text-sm font-medium">{t('chains.task_form.error_message')}</Label>
        <Textarea
          rows={3}
          value={task.prompt_template || ''}
          onChange={(e: React.ChangeEvent<HTMLTextAreaElement>) =>
            onChange({ prompt_template: e.target.value })
          }
          placeholder="Error: Validation failed because..."
        />
        <P className="text-text-muted mt-1 text-xs">{t('chains.task_form.error_message_help')}</P>
      </Panel>
    );
  }

  // noop / default
  if (task.handler === HandleNoop) {
    return (
      <Panel variant="surface" className="p-4">
        <div className="text-text-muted text-sm">{t('chains.task_form.noop_description')}</div>
      </Panel>
    );
  }

  return (
    <Panel variant="surface" className="p-4">
      <div className="text-text-muted text-sm">{t('chains.task_form.no_additional_config')}</div>
    </Panel>
  );
}
