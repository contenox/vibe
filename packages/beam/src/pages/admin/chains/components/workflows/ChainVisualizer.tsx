import {
  AddNodeButton,
  Badge,
  Button,
  calculateLayout,
  Card,
  Checkbox,
  FormField,
  H3,
  Input,
  Label,
  P,
  Panel,
  Span,
  WorkflowNode,
  WorkflowVisualizer,
} from '@contenox/ui';
import { Settings, X } from 'lucide-react';
import React, { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { BranchCompose, ChainDefinition, ChainTask } from '../../../../../lib/types';
import TaskForm from '../TaskForm/TaskForm';
import ComposeEditorPanel from './ComposeEditorPanel';
import EnhancedWorkflowEdge from './EnhancedWorkflowEdge';

type LayoutDirection = 'horizontal' | 'vertical';

interface ChainVisualizerProps {
  chain: ChainDefinition;
  selectedTaskId?: string | null;
  onTaskSelect: (task: ChainTask) => void;
  onTaskEdit?: (taskId: string) => void;
  onAddTask?: (afterTaskId: string, beforeTaskId?: string) => void;
  onTaskChange?: (taskId: string, updates: Partial<ChainTask>) => void;
  onTaskDelete?: (taskId: string) => void;
  onChainChange?: (updates: Partial<ChainDefinition>) => void;
}

function statusFor(task: ChainTask): 'default' | 'success' | 'error' | 'warning' {
  if (task.transition.on_failure) return 'warning';
  if (task.transition.branches.length > 1) return 'success';
  return 'default';
}

function boundsFromNodePositions(
  nodePositions: Record<string, { x: number; y: number; width: number; height: number }>,
) {
  const ids = Object.keys(nodePositions);
  if (ids.length === 0) return { x: 0, y: 0, width: 800, height: 480 };
  let minX = Infinity,
    minY = Infinity,
    maxX = -Infinity,
    maxY = -Infinity;
  for (const id of ids) {
    const p = nodePositions[id];
    minX = Math.min(minX, p.x);
    minY = Math.min(minY, p.y);
    maxX = Math.max(maxX, p.x + p.width);
    maxY = Math.max(maxY, p.y + p.height);
  }
  const PAD = 60;
  return {
    x: minX - PAD,
    y: minY - PAD,
    width: maxX - minX + PAD * 2,
    height: maxY - minY + PAD * 2,
  };
}

const ChainVisualizer: React.FC<ChainVisualizerProps> = ({
  chain,
  selectedTaskId,
  onTaskSelect,
  onAddTask,
  onTaskChange,
  onTaskDelete,
  onChainChange,
}) => {
  const { t } = useTranslation();
  const [editingTask, setEditingTask] = useState<ChainTask | null>(null);
  const [showChainConfig, setShowChainConfig] = useState(false);
  const [direction] = useState<LayoutDirection>('horizontal');
  const [editingCompose, setEditingCompose] = useState<{
    sourceTaskId: string;
    targetTaskId: string;
    composeConfig?: BranchCompose;
  } | null>(null);

  // NODES - including virtual end nodes
  const { nodes, endNodeMappings } = useMemo(() => {
    const taskNodes = chain.tasks.map(task => ({
      id: task.id,
      label: task.id,
      type: task.handler,
      description: task.description,
      metadata: {
        branches: task.transition.branches.length,
        status: statusFor(task),
      },
    }));

    // Create virtual end nodes and track mappings
    const endNodeMappings: Record<string, string> = {};
    const endNodes: Array<{
      id: string;
      label: string;
      type: string;
      description: string;
      metadata: { status: "default"; isEndNode: boolean; [key: string]: unknown };
    }> = [];

    chain.tasks.forEach(task => {
      const hasEndTransition = task.transition.branches.some(b => b.goto === 'end');
      if (hasEndTransition) {
        const endNodeId = `end-${task.id}`;
        endNodeMappings[task.id] = endNodeId;
        endNodes.push({
          id: endNodeId,
          label: 'end',
          type: 'end',
          description: 'Workflow termination',
          metadata: { status: 'default', isEndNode: true },
        });
      }
    });

    return { nodes: [...taskNodes, ...endNodes], endNodeMappings };
  }, [chain.tasks]);

  // EDGES (including edges to end nodes)
  const calcEdges = useMemo(() => {
    const edges: Array<{
      from: string;
      to: string;
      label: string;
      isError?: boolean;
      fromType: string;
    }> = [];

    chain.tasks.forEach(task => {
      // Add failure edge if it exists and doesn't go to "end"
      if (task.transition.on_failure && task.transition.on_failure !== 'end') {
        edges.push({
          from: task.id,
          to: task.transition.on_failure,
          label: t('workflow.on_failure'),
          isError: true,
          fromType: task.handler,
        });
      }

      // Add all branch edges including those to "end"
      task.transition.branches.forEach(branch => {
        if (branch.goto) {
          let targetId = branch.goto;

          // For transitions to "end", use the virtual end node
          if (branch.goto === 'end' && endNodeMappings[task.id]) {
            targetId = endNodeMappings[task.id];
          }

          edges.push({
            from: task.id,
            to: targetId,
            label: branch.when || t('workflow.default_branch'),
            fromType: task.handler,
          });
        }
      });
    });

    return edges;
  }, [chain.tasks, t, endNodeMappings]);

  const layoutResult = useMemo(
    () => calculateLayout(nodes, calcEdges, direction),
    [nodes, calcEdges, direction],
  );

  const filteredAddButtons = useMemo(() => {
    return layoutResult.addButtons.filter(button => {
      return (
        !button.fromNodeId.startsWith('end-') &&
        !button.toNodeId?.startsWith('end-') &&
        button.toNodeId !== 'end'
      );
    });
  }, [layoutResult.addButtons]);

  const { nodePositions, edges } = layoutResult;
  const contentBounds = useMemo(() => boundsFromNodePositions(nodePositions), [nodePositions]);

  const selectedTask = selectedTaskId ? chain.tasks.find(task => task.id === selectedTaskId) : null;
  const hasTasks = chain.tasks.length > 0;

  const computeAvailableVariables = (): string[] => {
    const variables = ['input'];
    chain.tasks.forEach(task => variables.push(task.id));
    return variables;
  };

  const handleNodeSelect = (nodeId: string) => {
    if (nodeId.startsWith('end-')) return; // ignore virtual end nodes
    const task = chain.tasks.find(t => t.id === nodeId);
    if (task) onTaskSelect(task);
  };

  const handleNodeAdd = (fromNodeId?: string, toNodeId?: string) => {
    onAddTask?.(fromNodeId || 'start', toNodeId);
  };

  const handleTaskFormChange = (updatedTask: ChainTask) => {
    if (editingTask) {
      onTaskChange?.(editingTask.id, updatedTask);
      setEditingTask(updatedTask);
    }
  };

  const handleTaskFormDelete = () => {
    if (editingTask) {
      onTaskDelete?.(editingTask.id);
      setEditingTask(null);
    }
  };

  const handleComposeEdit = (sourceTaskId: string, targetTaskId: string) => {
    const sourceTask = chain.tasks.find(task => task.id === sourceTaskId);
    if (!sourceTask) return;

    const actualTargetId = targetTaskId.startsWith('end-') ? 'end' : targetTaskId;

    const branch = sourceTask.transition.branches.find(b => b.goto === actualTargetId);

    setEditingCompose({
      sourceTaskId,
      targetTaskId: actualTargetId,
      composeConfig: branch?.compose,
    });
  };

  const handleComposeSave = (composeConfig: BranchCompose) => {
    if (!editingCompose) return;

    const updatedTasks = chain.tasks.map(task => {
      if (task.id === editingCompose.sourceTaskId) {
        const updatedBranches = task.transition.branches.map(branch => {
          const isMatchingBranch = branch.goto === editingCompose.targetTaskId;
          return isMatchingBranch ? { ...branch, compose: composeConfig } : branch;
        });
        return { ...task, transition: { ...task.transition, branches: updatedBranches } };
      }
      return task;
    });

    onChainChange?.({ ...chain, tasks: updatedTasks });
    setEditingCompose(null);
  };

  const handleComposeDelete = () => {
    if (!editingCompose) return;

    const updatedTasks = chain.tasks.map(task => {
      if (task.id !== editingCompose.sourceTaskId) return task;
      const updatedBranches = task.transition.branches.map(branch => {
        if (branch.goto !== editingCompose.targetTaskId) return branch;
        const { compose, ...rest } = branch;
        return rest;
      });
      return { ...task, transition: { ...task.transition, branches: updatedBranches } };
    });

    onChainChange?.({ ...chain, tasks: updatedTasks });
    setEditingCompose(null);
  };

  return (
    <div className="flex h-full">
      {/* MAIN CANVAS */}
      <div className="relative min-h-0 min-w-0 flex-1 overflow-hidden">
        {hasTasks ? (
          <WorkflowVisualizer
            debug={!!chain.debug}
            height="100%"
            className="h-full"
            contentBounds={contentBounds}
            initialZoom={1}>
            {/* EDGES */}
            {edges.map((e, i) => {
              const src = nodePositions[e.from];
              const dst = nodePositions[e.to];
              if (!src || !dst) return null;
              if (e.to === 'end') return null; // skip literal 'end' edges

              const sourceTask = chain.tasks.find(task => task.id === e.from);
              const actualTargetId = e.to.startsWith('end-') ? 'end' : e.to;
              const branch = sourceTask?.transition.branches.find(b => b.goto === actualTargetId);
              const hasCompose = !!branch?.compose;
              const composeStrategy = branch?.compose?.strategy ?? undefined;

              return (
                <g key={`edge-${i}`}>
                  <EnhancedWorkflowEdge
                    source={src}
                    target={dst}
                    label={e.label}
                    direction={direction}
                    isError={e.isError}
                    isHighlighted={selectedTaskId === e.from || selectedTaskId === e.to}
                    addButtonPositions={filteredAddButtons.map(b => ({ x: b.x, y: b.y }))}
                    hasCompose={hasCompose}
                    composeStrategy={composeStrategy}
                    onComposeClick={() => handleComposeEdit(e.from, e.to)} // click label → open compose
                  />
                </g>
              );
            })}

            {/* "+" ADD BUTTONS */}
            {onAddTask &&
              filteredAddButtons.map((b, i) => (
                <AddNodeButton
                  key={`add-${i}`}
                  x={b.x}
                  y={b.y}
                  onClick={() => handleNodeAdd(b.fromNodeId, b.toNodeId)}
                />
              ))}

            {/* NODES (incl. virtual end nodes) */}
            {nodes.map(n => {
              const pos = nodePositions[n.id];
              if (!pos) return null;
              const isEndNode = n.id.startsWith('end-') || n.type === 'end';

              return (
                <WorkflowNode
                  key={n.id}
                  id={n.id}
                  label={n.label}
                  type={n.type}
                  description={n.description}
                  metadata={n.metadata}
                  position={pos}
                  isSelected={selectedTaskId === n.id}
                  onClick={handleNodeSelect}
                  className={isEndNode ? 'workflow-end-node' : ''}
                />
              );
            })}
          </WorkflowVisualizer>
        ) : (
          <Card className="m-2 flex flex-1 items-center justify-center">
            <div className="p-8 text-center">
              <H3 className="mb-2 text-lg font-semibold">{t('workflow.no_tasks_title')}</H3>
              <P className="text-muted-foreground mb-4 text-sm">
                {t('workflow.no_tasks_description')}
              </P>
              {onAddTask && (
                <Button variant="primary" onClick={() => onAddTask('start')}>
                  {t('workflow.add_first_task')}
                </Button>
              )}
            </div>
          </Card>
        )}
      </div>

      {/* SIDE PANEL */}
      {(showChainConfig || editingTask || selectedTask || editingCompose) && (
        <div className="bg-background/50 flex min-h-0 w-[420px] min-w-[420px] flex-col overflow-hidden border-l">
          {showChainConfig && (
            <Panel className="flex min-h-0 flex-1 flex-col overflow-hidden">
              <div className="flex items-center justify-between border-b p-4">
                <H3 className="text-lg font-semibold">{t('chains.chain_config')}</H3>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => setShowChainConfig(false)}
                  className="h-8 w-8 p-0"
                  aria-label={t('common.close')}>
                  <X className="h-4 w-4" />
                </Button>
              </div>
              <div className="flex-1 space-y-4 overflow-auto p-4">
                <FormField label={t('chains.form_id')} required>
                  <Input
                    value={chain.id}
                    onChange={e => onChainChange?.({ id: e.target.value })}
                    className="text-sm"
                  />
                </FormField>
                <FormField label={t('chains.form_description')}>
                  <Input
                    value={chain.description}
                    onChange={e => onChainChange?.({ description: e.target.value })}
                    placeholder={t('chains.description_placeholder')}
                    className="text-sm"
                  />
                </FormField>
                <FormField label={t('chains.token_limit')}>
                  <Input
                    type="number"
                    value={chain.token_limit ?? 4096}
                    onChange={e =>
                      onChainChange?.({ token_limit: parseInt(e.target.value) || 4096 })
                    }
                    className="text-sm"
                  />
                </FormField>
                <div className="flex items-center gap-3 pt-2">
                  <Checkbox
                    checked={!!chain.debug}
                    onChange={(e: React.ChangeEvent<HTMLInputElement>) =>
                      onChainChange?.({ debug: e.target.checked })
                    }
                  />
                  <Label className="cursor-pointer text-sm font-medium">
                    {t('chains.enable_debug')}
                  </Label>
                </div>
              </div>
            </Panel>
          )}

          {editingCompose && (
            <ComposeEditorPanel
              sourceTaskId={editingCompose.sourceTaskId}
              targetTaskId={editingCompose.targetTaskId}
              composeConfig={editingCompose.composeConfig}
              onSave={handleComposeSave}
              onCancel={() => setEditingCompose(null)}
              onDelete={handleComposeDelete}
              availableVariables={computeAvailableVariables()}
            />
          )}

          {!editingCompose && editingTask && (
            <div className="flex min-h-0 flex-1">
              <TaskForm
                task={editingTask}
                onChange={handleTaskFormChange}
                onSave={() => setEditingTask(null)}
                onCancel={() => setEditingTask(null)}
                onDelete={onTaskDelete ? handleTaskFormDelete : undefined}
                availableVariables={computeAvailableVariables()}
              />
            </div>
          )}

          {!editingCompose && !editingTask && selectedTask && (
            <Panel className="flex min-h-0 flex-1 flex-col overflow-hidden">
              <div className="flex items-center justify-between border-b p-4">
                <H3 className="truncate text-lg font-semibold">{selectedTask.id}</H3>
                <div className="flex shrink-0 gap-2">
                  {onTaskChange && (
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => setEditingTask(selectedTask)}>
                      {t('common.edit')}
                    </Button>
                  )}
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => onTaskSelect(selectedTask)}
                    className="h-8 w-8 shrink-0 p-0"
                    aria-label={t('common.close')}>
                    <X className="h-4 w-4" />
                  </Button>
                </div>
              </div>
              <div className="flex-1 space-y-4 overflow-auto p-4">
                <div>
                  <Label className="text-sm font-medium">{t('chains.task_handler')}</Label>
                  <div className="bg-secondary/20 mt-1 rounded px-3 py-2 text-sm">
                    {selectedTask.handler}
                  </div>
                </div>
                {selectedTask.description && (
                  <div>
                    <Label className="text-sm font-medium">{t('chains.task_description')}</Label>
                    <P className="text-muted-foreground mt-1 text-sm">{selectedTask.description}</P>
                  </div>
                )}
                {selectedTask.input_var && (
                  <div>
                    <Label className="text-sm font-medium">{t('chains.input_variable')}</Label>
                    <div className="bg-secondary/20 mt-1 rounded px-3 py-2 font-mono text-sm">
                      {selectedTask.input_var}
                    </div>
                  </div>
                )}
                {selectedTask.prompt_template && (
                  <div>
                    <Label className="text-sm font-medium">{t('chains.prompt_template')}</Label>
                    <div className="bg-secondary/20 mt-1 max-h-40 overflow-auto rounded p-3">
                      <pre className="text-sm break-words whitespace-pre-wrap">
                        {selectedTask.prompt_template}
                      </pre>
                    </div>
                  </div>
                )}
                <div>
                  <Label className="text-sm font-medium">{t('chains.transitions')}</Label>
                  <Card className="mt-2 space-y-3 p-3">
                    {selectedTask.transition.on_failure && (
                      <div className="flex items-center justify-between">
                        <Span className="text-sm font-medium">{t('workflow.on_failure')}</Span>
                        <Badge variant="error" size="sm">
                          {selectedTask.transition.on_failure}
                        </Badge>
                      </div>
                    )}
                    {selectedTask.transition.branches.map((branch, index) => (
                      <div
                        key={index}
                        className="flex items-center justify-between border-b pb-3 last:border-b-0 last:pb-0">
                        <div className="mr-3 min-w-0 flex-1">
                          <Span className="block truncate text-sm font-medium">
                            {branch.when || t('workflow.default_branch')}
                          </Span>
                          <div className="text-muted-foreground text-xs">
                            {branch.operator || 'default'}
                          </div>
                        </div>
                        <Badge variant="default" size="sm" className="shrink-0">
                          {branch.goto}
                        </Badge>
                        {branch.compose && (
                          <div className="mt-1 text-xs">
                            <div>
                              <strong>compose.with_var:</strong>{' '}
                              {branch.compose.with_var || '(none)'}
                            </div>
                            <div>
                              <strong>compose.strategy:</strong>{' '}
                              {branch.compose.strategy || '(default)'}
                            </div>
                          </div>
                        )}
                      </div>
                    ))}
                  </Card>
                </div>
              </div>
            </Panel>
          )}
        </div>
      )}

      {/* CONFIG BUTTON */}
      {!showChainConfig && !editingTask && !editingCompose && (
        <div className="absolute right-6 bottom-6">
          <Button
            size="sm"
            variant="secondary"
            onClick={() => setShowChainConfig(true)}
            className="bg-background/95 shadow-lg backdrop-blur-sm">
            <Settings className="mr-2 h-4 w-4" />
            {t('chains.chain_config')}
          </Button>
        </div>
      )}
    </div>
  );
};

export default ChainVisualizer;
