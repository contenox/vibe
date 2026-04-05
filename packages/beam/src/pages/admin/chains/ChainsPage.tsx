// src/pages/admin/chains/ChainsPage.tsx
import { Button, Panel, Section, Tabs } from '@contenox/ui';
import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useSearchParams } from 'react-router-dom';
import { ErrorState, LoadingState } from '../../../components/LoadingState';
import { Fill, Page } from '../../../components/Page';
import { useListFiles } from '../../../hooks/useFiles';
import {
  useChain,
  useCreateChain,
  useUpdateChain,
} from '../../../hooks/useChains';
import { isChainLikeVfsPath } from '../../../lib/chainPaths';
import type { ChainDefinition, ChainTask } from '../../../lib/types';
import ChainJsonEditor from './components/ChainJsonEditor';
import ChainsList from './components/ChainsList';
import ChainVisualizer from './components/workflows/ChainVisualizer';

interface Tab {
  id: 'list' | 'visualize' | 'json';
  label: string;
  disabled?: boolean;
}

export default function ChainsPage() {
  const { t } = useTranslation();
  const [searchParams, setSearchParams] = useSearchParams();
  const pathParam = searchParams.get('path') ?? '';

  const { data: files = [], isLoading: filesLoading, error: filesError, refetch: refetchFiles } =
    useListFiles();
  const chainPaths = useMemo(
    () => files.filter(f => isChainLikeVfsPath(f.path)).map(f => f.path),
    [files],
  );

  const { data: loadedChain, isLoading: chainLoading, error: chainError, refetch: refetchChain } =
    useChain(pathParam);

  const [selectedChain, setSelectedChain] = useState<ChainDefinition | null>(null);
  const [selectedTaskId, setSelectedTaskId] = useState<string | null>(null);
  const [activeTab, setActiveTab] = useState<string>('list');
  const [jsonDraft, setJsonDraft] = useState<string | null>(null);
  const [pendingVfsPath, setPendingVfsPath] = useState<string | null>(null);
  const [isNewDraft, setIsNewDraft] = useState(false);

  const editorPath = pathParam || pendingVfsPath || '';
  const createChain = useCreateChain();
  const updateChain = useUpdateChain(editorPath);

  useEffect(() => {
    if (pathParam && loadedChain) {
      setSelectedChain(loadedChain);
      setPendingVfsPath(null);
      setIsNewDraft(false);
    }
  }, [pathParam, loadedChain]);

  const handleCreateNew = () => {
    const vfsPath = `chain-${Date.now()}.json`;
    const newChain: ChainDefinition = {
      id: `chain_${Date.now()}`,
      description: '',
      tasks: [
        {
          id: `task_${Date.now()}`,
          description: '',
          handler: 'prompt_to_string',
          prompt_template: '',
          transition: {
            branches: [{ when: '', operator: 'default', goto: 'end' }],
            on_failure: '',
          },
        },
      ],
      token_limit: 4096,
      debug: false,
    };
    setPendingVfsPath(vfsPath);
    setIsNewDraft(true);
    setSearchParams({}, { replace: true });
    setSelectedChain(newChain);
    setSelectedTaskId(null);
    setActiveTab('visualize');
  };

  const handleSelectPath = (vfsPath: string) => {
    setSearchParams({ path: vfsPath }, { replace: true });
    setPendingVfsPath(null);
    setIsNewDraft(false);
    setSelectedTaskId(null);
    setActiveTab('visualize');
  };

  const persistChain = useCallback(
    async (chain: ChainDefinition) => {
      const targetPath = pathParam || pendingVfsPath;
      if (!targetPath) return;

      if (isNewDraft && pendingVfsPath) {
        await createChain.mutateAsync({ vfsPath: pendingVfsPath, chain });
        setIsNewDraft(false);
        setSearchParams({ path: pendingVfsPath }, { replace: true });
        return;
      }

      await updateChain.mutateAsync(chain);
      setSelectedChain(chain);
    },
    [pathParam, pendingVfsPath, isNewDraft, createChain, updateChain, setSearchParams],
  );

  const handleSave = useCallback(async () => {
    if (!selectedChain) return;

    if (activeTab === 'json' && jsonDraft != null) {
      try {
        const parsed = JSON.parse(jsonDraft) as ChainDefinition;
        await persistChain(parsed);
        return;
      } catch (e) {
        const msg = e instanceof Error ? e.message : String(e);
        alert(`Invalid JSON: ${msg}`);
        return;
      }
    }

    await persistChain(selectedChain);
  }, [activeTab, jsonDraft, persistChain, selectedChain]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
        e.preventDefault();
        void handleSave();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [handleSave]);

  const handleTaskSelect = (task: ChainTask) => setSelectedTaskId(task.id);

  const handleAddTask = (fromTaskId: string, toNodeId?: string) => {
    if (!selectedChain) return;

    const newTask: ChainTask = {
      id: `task_${Date.now()}`,
      description: '',
      handler: 'prompt_to_string',
      prompt_template: '',
      transition: {
        branches: [{ when: '', operator: 'default', goto: 'end' }],
        on_failure: '',
      },
    };

    const updatedTasks = [...selectedChain.tasks, newTask];

    if (toNodeId) {
      const updatedChainWithInsertion: ChainDefinition = {
        ...selectedChain,
        tasks: updatedTasks.map(task => {
          if (task.id === fromTaskId) {
            const branches = task.transition.branches.map(b =>
              b.goto === toNodeId ? { ...b, goto: newTask.id } : b,
            );
            return { ...task, transition: { ...task.transition, branches } };
          }
          return task;
        }),
      };

      const finalChain: ChainDefinition = {
        ...updatedChainWithInsertion,
        tasks: updatedChainWithInsertion.tasks.map(task =>
          task.id === newTask.id
            ? {
                ...task,
                transition: {
                  ...task.transition,
                  branches: [{ when: '', operator: 'default', goto: toNodeId }],
                },
              }
            : task,
        ),
      };

      setSelectedChain(finalChain);
    } else {
      const updatedChainWithFanOut: ChainDefinition = {
        ...selectedChain,
        tasks: updatedTasks.map(task => {
          if (task.id === fromTaskId) {
            return {
              ...task,
              transition: {
                ...task.transition,
                branches: [
                  ...task.transition.branches,
                  { when: '', operator: 'default', goto: newTask.id },
                ],
              },
            };
          }
          return task;
        }),
      };

      setSelectedChain(updatedChainWithFanOut);
    }

    setSelectedTaskId(newTask.id);
  };

  const handleTaskChange = (taskId: string, updates: Partial<ChainTask>) => {
    if (!selectedChain) return;
    const tasks = selectedChain.tasks.map(task =>
      task.id === taskId ? { ...task, ...updates } : task,
    );
    setSelectedChain({ ...selectedChain, tasks });
  };

  const handleTaskDelete = (taskId: string) => {
    if (!selectedChain) return;
    const tasks = selectedChain.tasks.filter(task => task.id !== taskId);
    setSelectedChain({ ...selectedChain, tasks });
    if (selectedTaskId === taskId) setSelectedTaskId(null);
  };

  const handleChainChange = (updates: Partial<ChainDefinition>) => {
    if (!selectedChain) return;
    setSelectedChain({ ...selectedChain, ...updates });
  };

  const refetch = useCallback(() => {
    void refetchFiles();
    if (pathParam) void refetchChain();
  }, [refetchFiles, refetchChain, pathParam]);

  const tabs: Tab[] = [
    { id: 'list', label: t('chains.tabs.list') },
    { id: 'visualize', label: t('chains.tabs.visualize'), disabled: !selectedChain },
    { id: 'json', label: t('chains.tabs.json'), disabled: !selectedChain },
  ];

  const isLoading = filesLoading || (!!pathParam && chainLoading);
  const error = filesError ?? chainError ?? null;

  if (isLoading) return <LoadingState message={t('chains.loading')} />;
  if (error)
    return <ErrorState error={error} onRetry={refetch} title={t('chains.loading_error')} />;

  return (
    <Page
      bodyScroll="hidden"
      header={
        <Section title={t('chains.title')} description={t('chains.page_description')}>
          <div className="mb-4 flex items-center justify-between gap-3">
            <Tabs tabs={tabs} activeTab={activeTab} onTabChange={setActiveTab} />

            <div className="flex items-center gap-2">
              {activeTab === 'list' && (
                <Button variant="primary" onClick={handleCreateNew}>
                  {t('chains.create_new')}
                </Button>
              )}

              {(activeTab === 'visualize' || activeTab === 'json') && selectedChain && (
                <>
                  <Button
                    variant="secondary"
                    onClick={() => setActiveTab(activeTab === 'visualize' ? 'json' : 'visualize')}>
                    {activeTab === 'visualize'
                      ? t('chains.json_editor')
                      : t('chains.visual_editor')}
                  </Button>

                  <Button
                    variant="primary"
                    onClick={handleSave}
                    disabled={
                      createChain.isPending ||
                      updateChain.isPending ||
                      (!pathParam && !pendingVfsPath)
                    }>
                    {createChain.isPending || updateChain.isPending
                      ? t('common.saving')
                      : t('common.save')}
                  </Button>
                </>
              )}
            </div>
          </div>
        </Section>
      }>
      {activeTab === 'list' && (
        <Fill className="flex min-h-0 min-w-0">
          <ChainsList
            paths={chainPaths}
            error={null}
            onSelectPath={handleSelectPath}
            onCreate={handleCreateNew}
          />
        </Fill>
      )}

      {activeTab === 'visualize' && selectedChain && (
        <Fill>
          <ChainVisualizer
            chain={selectedChain}
            selectedTaskId={selectedTaskId}
            onTaskSelect={handleTaskSelect}
            onTaskEdit={setSelectedTaskId}
            onAddTask={handleAddTask}
            onTaskChange={handleTaskChange}
            onTaskDelete={handleTaskDelete}
            onChainChange={handleChainChange}
          />
        </Fill>
      )}

      {activeTab === 'json' && selectedChain && (
        <Fill className="flex min-h-0 min-w-0 p-4">
          <ChainJsonEditor chain={selectedChain} onChangeText={setJsonDraft} className="flex-1" />
        </Fill>
      )}

      {(activeTab === 'visualize' || activeTab === 'json') && !selectedChain && (
        <Panel variant="surface" className="m-6 py-12 text-center">
          <h3 className="mb-2 text-lg font-semibold">{t('chains.no_chain_selected')}</h3>
          <p className="text-muted-foreground mb-4">{t('chains.select_or_create_chain')}</p>
          <Button variant="primary" onClick={handleCreateNew}>
            {t('chains.create_new')}
          </Button>
        </Panel>
      )}
    </Page>
  );
}
