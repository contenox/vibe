import { cn } from "../utils";
import { ProgressBar } from "./ProgressBar";
import { P } from "./Typography";

export type Status =
  | "planned"
  | "in-progress"
  | "completed"
  | "error"
  | "warning"
  | "info";
export type Size = "sm" | "md" | "lg";

export interface StatusIndicatorProps {
  status: Status;
  label?: string;
  progress?: number;
  size?: Size;
  className?: string;
  showIcon?: boolean;
}

export function StatusIndicator({
  status,
  label,
  progress,
  size = "md",
  className,
  showIcon = false,
}: StatusIndicatorProps) {
  const statusConfig = {
    planned: {
      color: "bg-surface-400 dark:bg-dark-surface-500",
      text: "text-text-muted dark:text-dark-text-muted",
      icon: "○",
    },
    "in-progress": {
      color: "bg-yellow-500 dark:bg-dark-warning-500",
      text: "text-yellow-700 dark:text-dark-warning-300",
      icon: "⟳",
    },
    completed: {
      color: "bg-green-500 dark:bg-dark-success-500",
      text: "text-green-700 dark:text-dark-success-300",
      icon: "✓",
    },
    error: {
      color: "bg-red-500 dark:bg-dark-error-500",
      text: "text-red-700 dark:text-dark-error-300",
      icon: "✗",
    },
    warning: {
      color: "bg-orange-500 dark:bg-dark-warning-500",
      text: "text-orange-700 dark:text-dark-warning-300",
      icon: "⚠",
    },
    info: {
      color: "bg-blue-500 dark:bg-dark-primary-500",
      text: "text-blue-700 dark:text-dark-primary-300",
      icon: "ℹ",
    },
  };

  return (
    <div className={cn("flex items-center gap-3", className)}>
      {/* Status indicator */}
      <div className="flex items-center gap-2">
        <span
          className={cn("w-2 h-2 rounded-full", statusConfig[status].color)}
        />
        {showIcon && (
          <span className={cn("text-xs", statusConfig[status].text)}>
            {statusConfig[status].icon}
          </span>
        )}
      </div>

      {/* Label and progress */}
      <div className="flex-1">
        {label && (
          <P
            variant="caption"
            className={cn(
              "uppercase tracking-wide",
              statusConfig[status].text,
              size === "sm" ? "text-xs" : "text-sm",
            )}
          >
            {label}
          </P>
        )}

        {typeof progress === "number" && (
          <ProgressBar
            value={progress}
            palette={
              status === "completed"
                ? "success"
                : status === "error"
                  ? "error"
                  : status === "warning"
                    ? "warning"
                    : status === "in-progress"
                      ? "warning"
                      : "neutral"
            }
            className={size === "sm" ? "h-1.5" : "h-2"}
          />
        )}
      </div>
    </div>
  );
}
