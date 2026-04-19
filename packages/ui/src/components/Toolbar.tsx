import { forwardRef } from "react";
import { cn } from "../utils";
import { Span } from "./Typography";
import { Badge } from "./Badge";
import { Tooltip } from "./Tooltip";

export const Toolbar = forwardRef<HTMLDivElement, React.HTMLAttributes<HTMLDivElement>>(
  function Toolbar({ className, ...props }, ref) {
    return (
      <div
        ref={ref}
        role="toolbar"
        className={cn(
          "bg-surface-50 dark:bg-dark-surface-200",
          "text-text dark:text-dark-text",
          "flex shrink-0 flex-wrap items-center gap-x-3 gap-y-2",
          "border-b border-border",
          "px-3 py-2",
          className,
        )}
        {...props}
      />
    );
  },
);

export const ToolbarSection = forwardRef<HTMLDivElement, React.HTMLAttributes<HTMLDivElement>>(
  function ToolbarSection({ className, ...props }, ref) {
    return (
      <div
        ref={ref}
        className={cn(
          "flex min-w-0 flex-1 flex-wrap items-center gap-x-2 gap-y-1.5 sm:gap-x-3",
          className,
        )}
        {...props}
      />
    );
  },
);

interface ToolbarItemProps extends React.HTMLAttributes<HTMLDivElement> {
  label: string;
  tooltip?: string;
}

export function ToolbarItem({ label, tooltip, children, className, ...props }: ToolbarItemProps) {
  return (
    <div className={cn("flex items-center gap-1.5", className)} {...props}>
      <Span variant="muted" className="shrink-0 text-xs sm:text-sm">
        {label}
      </Span>
      {tooltip && (
        <span className="shrink-0">
          <Tooltip content={tooltip} position="top">
            <Badge variant="outline" size="sm" className="cursor-help px-1.5">
              ?
            </Badge>
          </Tooltip>
        </span>
      )}
      {children}
    </div>
  );
}

export const ToolbarActions = forwardRef<HTMLDivElement, React.HTMLAttributes<HTMLDivElement>>(
  function ToolbarActions({ className, ...props }, ref) {
    return (
      <div
        ref={ref}
        className={cn("flex shrink-0 items-center gap-1.5", className)}
        {...props}
      />
    );
  },
);
