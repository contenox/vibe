// src/pages/admin/chains/components/workflows/EnhancedWorkflowEdge.tsx
import { LayoutDirection, NodePosition, WorkflowEdge } from '@contenox/ui';
import React from 'react';

interface EnhancedWorkflowEdgeProps {
  source: NodePosition;
  target: NodePosition;
  label: string;
  direction: LayoutDirection;
  isError?: boolean;
  isHighlighted?: boolean;
  addButtonPositions?: Array<{ x: number; y: number }>;
  hasCompose?: boolean;
  composeStrategy?: string | null; // upstream can be null
  onComposeClick?: () => void;
}

export const EnhancedWorkflowEdge: React.FC<EnhancedWorkflowEdgeProps> = ({
  source,
  target,
  hasCompose,
  composeStrategy,
  onComposeClick,
  ...props
}) => {
  return (
    <g className="enhanced-workflow-edge">
      <WorkflowEdge
        source={source}
        target={target}
        hasCompose={hasCompose}
        composeStrategy={composeStrategy ?? undefined} // coerce null → undefined
        onComposeClick={onComposeClick}
        {...props}
      />
    </g>
  );
};

export default EnhancedWorkflowEdge;
