import { Panel } from "../Panel";
import { GitBranch } from "lucide-react";
import React from "react";
import { cn } from "../../utils";

export interface WorkflowNodeProps {
  id: string;
  label: string;
  type: string;
  description?: string;
  metadata?: {
    branches?: number;
    status?: "default" | "success" | "error" | "warning";
    [key: string]: unknown;
  };
  position: { x: number; y: number; width: number; height: number };
  isSelected?: boolean;
  onClick?: (id: string) => void;
  className?: string;
}

export const WorkflowNode: React.FC<WorkflowNodeProps> = ({
  id,
  label,
  type,
  description,
  metadata,
  position,
  isSelected = false,
  onClick,
  className,
}) => {
  const { x, y, width, height } = position;

  const handleClick = () => {
    onClick?.(id);
  };

  const statusStrokes = {
    default: "stroke-surface-300 dark:stroke-dark-surface-600",
    success: "stroke-success-500 dark:stroke-dark-success-500",
    error: "stroke-error-500 dark:stroke-dark-error-500",
    warning: "stroke-warning-500 dark:stroke-dark-warning-500",
  } as const;

  const status = metadata?.status || "default";

  return (
    <g
      transform={`translate(${x}, ${y})`}
      className={cn("cursor-pointer", className)}
      onClick={handleClick}
    >
      <rect
        width={width}
        height={height}
        rx="12"
        className={cn(
          "fill-surface-50 dark:fill-dark-surface-50 stroke-2 transition-all duration-300 ease-in-out",
          "shadow-md hover:shadow-lg",
          statusStrokes[status],
          isSelected ? "stroke-accent-500 dark:stroke-dark-accent-400" : "",
        )}
      />

      <foreignObject width={width} height={height}>
        <div className="flex h-full flex-col p-3">
          {/* Header */}
          <div className="flex items-start justify-between">
            <div className="grow overflow-hidden">
              <div
                className="truncate font-medium text-text dark:text-dark-text"
                title={label}
              >
                {label}
              </div>
              <div className="truncate text-sm text-text-muted dark:text-dark-text-muted">
                {type}
              </div>
            </div>
          </div>

          {/* Description */}
          {description && (
            <div className="mt-2 line-clamp-2 grow text-sm text-text-muted dark:text-dark-text-muted">
              {description}
            </div>
          )}

          {/* Metadata */}
          {metadata?.branches !== undefined && (
            <div className="mt-2 flex items-center justify-end text-xs text-text-muted dark:text-dark-text-muted">
              <GitBranch className="mr-1 h-3 w-3" />
              <span>
                {metadata.branches} branch{metadata.branches !== 1 && "es"}
              </span>
            </div>
          )}
        </div>
      </foreignObject>
    </g>
  );
};
