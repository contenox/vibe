import { GridLayout, Panel, Section, Spinner } from '@contenox/ui';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useSearchParams } from 'react-router-dom';
import { useChain } from '../../../../hooks/useChains';
import { ChainTask } from '../../../../lib/types';
import ChainEditor from './ChainEditor';
import ChainVisualizer from './workflows/ChainVisualizer';

/** Prefer navigating to `/chains?path=…` from the Chains page; this layout supports the same query param. */
export default function ChainDetailPage() {
  const [searchParams] = useSearchParams();
  const vfsPath = searchParams.get('path') ?? '';
  const { t } = useTranslation();
  const { data: chain, isLoading, error } = useChain(vfsPath);
  const [selectedTask, setSelectedTask] = useState<ChainTask | null>(null);
  const [taskToEdit, setTaskToEdit] = useState<string | null>(null);

  if (isLoading) {
    return (
      <Section className="flex justify-center py-10">
        <Spinner size="lg" />
      </Section>
    );
  }

  if (!vfsPath) {
    return (
      <Panel variant="surface" className="m-6">
        {t('chains.missing_path', 'Open a chain from Files or Chains (path query is required).')}
      </Panel>
    );
  }

  if (error || !chain) {
    return <Panel variant="error">{error?.message || t('chains.not_found')}</Panel>;
  }

  return (
    <GridLayout variant="body" className="h-full">
      <Section
        title={t('chains.editor_title', { id: chain.id })}
        className="flex h-full min-h-0 flex-col">
        <div className="grid h-full min-h-0 flex-1 grid-cols-1 gap-6 lg:grid-cols-2">
          <ChainEditor
            chain={chain}
            vfsPath={vfsPath}
            selectedTask={selectedTask}
            onTaskSelect={setSelectedTask}
            highlightTaskId={taskToEdit}
            onHighlightReset={() => setTaskToEdit(null)}
          />
          <div className="flex min-h-0 flex-col">
            <ChainVisualizer
              chain={chain}
              selectedTaskId={selectedTask?.id || null}
              onTaskSelect={task => {
                setSelectedTask(task);
                setTaskToEdit(null);
              }}
              onTaskEdit={taskId => {
                setTaskToEdit(taskId);
                setTimeout(() => {
                  const element = document.getElementById(`task-${taskId}`);
                  if (element) {
                    element.scrollIntoView({ behavior: 'smooth', block: 'center' });
                  }
                }, 100);
              }}
            />
          </div>
        </div>
      </Section>
    </GridLayout>
  );
}
