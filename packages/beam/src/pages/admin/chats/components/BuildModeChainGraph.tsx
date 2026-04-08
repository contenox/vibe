import {
  calculateLayout,
  InsetPanel,
  InsetPanelHeader,
  P,
  Span,
  WorkflowNode,
  WorkflowVisualizer,
} from '@contenox/ui';
import React, { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import type { ChainDefinition, ChainTask } from '../../../../lib/types';
import EnhancedWorkflowEdge from '../../chains/components/workflows/EnhancedWorkflowEdge';

type LayoutDirection = 'horizontal' | 'vertical';

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

export type BuildModeChainGraphProps = {
  chain: ChainDefinition;
  /** Shown above the canvas (e.g. executor-only hint). */
  caption?: string | null;
  isLoading?: boolean;
  error?: Error | null;
};

/**
 * Read-only workflow graph for build mode (same layout as chain editor, no editing).
 */
const BuildModeChainGraph: React.FC<BuildModeChainGraphProps> = ({
  chain,
  caption,
  isLoading,
  error,
}) => {
  const { t } = useTranslation();
  const direction: LayoutDirection = 'horizontal';

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

    const endNodeMappings: Record<string, string> = {};
    const endNodes: Array<{
      id: string;
      label: string;
      type: string;
      description: string;
      metadata: { status: 'default'; isEndNode: boolean; [key: string]: unknown };
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

  const calcEdges = useMemo(() => {
    const edges: Array<{
      from: string;
      to: string;
      label: string;
      isError?: boolean;
      fromType: string;
    }> = [];

    chain.tasks.forEach(task => {
      if (task.transition.on_failure && task.transition.on_failure !== 'end') {
        edges.push({
          from: task.id,
          to: task.transition.on_failure,
          label: t('workflow.on_failure'),
          isError: true,
          fromType: task.handler,
        });
      }

      task.transition.branches.forEach(branch => {
        if (branch.goto) {
          let targetId = branch.goto;
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

  const hasTasks = chain.tasks.length > 0;

  if (error) {
    return (
      <InsetPanel tone="muted" className="p-3">
        <P className="text-error-700 dark:text-dark-error text-sm">{error.message}</P>
      </InsetPanel>
    );
  }

  if (isLoading) {
    return (
      <InsetPanel tone="muted" className="flex min-h-[200px] items-center justify-center">
        <Span variant="muted" className="text-sm">
          {t('chat.build_graph_loading')}
        </Span>
      </InsetPanel>
    );
  }

  return (
    <InsetPanel className="min-h-0 flex-1">
      {caption ? (
        <InsetPanelHeader density="comfortable">
          <P className="text-text-secondary dark:text-dark-text-muted text-xs">{caption}</P>
        </InsetPanelHeader>
      ) : null}
      <div className="relative min-h-0 flex-1 overflow-hidden">
        {hasTasks ? (
          <WorkflowVisualizer
            debug={!!chain.debug}
            height="100%"
            className="h-full min-h-[240px]"
            contentBounds={contentBounds}
            initialZoom={1}
            scrollOnOverflow>
            {edges.map((e, i) => {
              const src = nodePositions[e.from];
              const dst = nodePositions[e.to];
              if (!src || !dst) return null;
              if (e.to === 'end') return null;

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
                    isHighlighted={false}
                    addButtonPositions={filteredAddButtons.map(b => ({ x: b.x, y: b.y }))}
                    hasCompose={hasCompose}
                    composeStrategy={composeStrategy}
                  />
                </g>
              );
            })}

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
                  isSelected={false}
                  onClick={() => {}}
                  className={isEndNode ? 'workflow-end-node' : ''}
                />
              );
            })}
          </WorkflowVisualizer>
        ) : (
          <div className="text-text-secondary dark:text-dark-text-muted flex h-full items-center justify-center p-6 text-sm">
            {t('chat.build_graph_empty')}
          </div>
        )}
      </div>
    </InsetPanel>
  );
};

export default BuildModeChainGraph;
