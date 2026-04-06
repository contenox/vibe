import {
  Button,
  Form,
  FormField,
  GridLayout,
  Panel,
  Section,
  Span,
  Spinner,
  Textarea,
} from '@contenox/ui';
import { useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useExecPrompt } from '../../../hooks/useExec';
import { useTaskEvents } from '../../../hooks/useTaskEvents';
import { createTaskEventRequestId } from '../../../lib/taskEvents';

export default function ExecPromptPage() {
  const { t } = useTranslation();
  const [prompt, setPrompt] = useState('');
  const [executedPrompt, setExecutedPrompt] = useState<string | null>(null);
  const [activeRequestId, setActiveRequestId] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  const cancelledRef = useRef(false);

  const { mutate: executePrompt, data, isPending, isError, error } = useExecPrompt();
  const liveTask = useTaskEvents(activeRequestId, { enabled: !!activeRequestId && isPending });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (prompt.trim()) {
      abortRef.current?.abort();
      const controller = new AbortController();
      const requestId = createTaskEventRequestId();
      cancelledRef.current = false;
      abortRef.current = controller;
      setActiveRequestId(requestId);
      executePrompt(
        { prompt, requestId, signal: controller.signal },
        {
          onSettled: () => {
            abortRef.current = null;
          },
        },
      );
      setExecutedPrompt(prompt);
    }
  };

  const handleCancel = () => {
    cancelledRef.current = true;
    abortRef.current?.abort();
    abortRef.current = null;
  };

  return (
    <GridLayout variant="body">
      <Section>
        <Form
          onSubmit={handleSubmit}
          title={t('prompt.title', 'Execute Prompt')}
          actions={
            isPending ? (
              <Button type="button" variant="outline" onClick={handleCancel}>
                {t('common.cancel', 'Cancel')}
              </Button>
            ) : (
              <Button type="submit" variant="primary">
                {t('prompt.execute', 'Execute')}
              </Button>
            )
          }>
          <FormField label={t('prompt.label', 'Prompt')} required>
            <Textarea
              value={prompt}
              onChange={e => setPrompt(e.target.value)}
              placeholder={t('prompt.placeholder', 'Enter your prompt')}
            />
          </FormField>
        </Form>
      </Section>

      <Section>
        {!executedPrompt && (
          <Panel>{t('prompt.invite', 'Enter a prompt to see the result.')}</Panel>
        )}

        {isPending && <Spinner size="lg" />}

        {isPending && (liveTask.status || liveTask.thinking || liveTask.content) && (
          <Panel variant="raised" className="space-y-3">
            {liveTask.status && (
              <Span variant="sectionTitle">{liveTask.status}</Span>
            )}
            {liveTask.thinking && (
              <Panel variant="surface" className="m-0 overflow-auto p-3">
                <pre className="text-text dark:text-dark-text text-sm whitespace-pre-wrap">
                  {liveTask.thinking}
                </pre>
              </Panel>
            )}
            {liveTask.content && <Span>{liveTask.content}</Span>}
          </Panel>
        )}

        {isError && !cancelledRef.current && (
          <Panel variant="error">
            {t('prompt.error', 'Execution failed')}: {error?.message}
          </Panel>
        )}

        {data && (
          <Panel variant="raised" className="space-y-2">
            <Span variant="sectionTitle">{'>> '}</Span>
            <Span>{data.response}</Span>
          </Panel>
        )}
      </Section>
    </GridLayout>
  );
}
