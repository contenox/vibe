import { forwardRef, useState } from "react";
import { ChevronDown, ExternalLink } from "lucide-react";
import { cn } from "../utils";
import { Badge } from "./Badge";
import { Span } from "./Typography";

type ToolCallStatus = "pending" | "running" | "success" | "error";

export interface ToolCallCardProps
  extends React.HTMLAttributes<HTMLDivElement> {
  /** Tool / hook name (e.g. "local_shell", "linear"). */
  tool: string;
  /** Short summary of what happened (e.g. "Created DEV-116"). */
  title: string;
  /** Execution status. */
  status?: ToolCallStatus;
  /** Icon shown before the tool name. */
  icon?: React.ReactNode;
  /** External link (e.g. Linear issue URL). */
  href?: string;
  /** Collapsible detail content (raw JSON, full output, etc.). */
  detail?: React.ReactNode;
  /** Duration string (e.g. "1.2s"). */
  duration?: string;
}

const statusBadge: Record<ToolCallStatus, { variant: "secondary" | "primary" | "success" | "error"; label: string }> = {
  pending: { variant: "secondary", label: "Pending" },
  running: { variant: "primary", label: "Running" },
  success: { variant: "success", label: "Done" },
  error: { variant: "error", label: "Error" },
};

export const ToolCallCard = forwardRef<HTMLDivElement, ToolCallCardProps>(
  function ToolCallCard(
    {
      className,
      tool,
      title,
      status = "success",
      icon,
      href,
      detail,
      duration,
      ...props
    },
    ref,
  ) {
    const [open, setOpen] = useState(false);
    const badge = statusBadge[status];

    return (
      <div
        ref={ref}
        className={cn(
          "rounded-lg border",
          "border-surface-200 dark:border-dark-surface-500",
          "bg-surface-50 dark:bg-dark-surface-200",
          "text-text dark:text-dark-text",
          "transition-colors",
          className,
        )}
        {...props}
      >
        {/* Header */}
        <div className="flex items-center gap-2 px-3 py-2">
          {icon && (
            <span className="text-text-muted dark:text-dark-text-muted shrink-0">
              {icon}
            </span>
          )}

          <Span
            variant="muted"
            className="shrink-0 font-mono text-xs"
          >
            {tool}
          </Span>

          <Span className="min-w-0 flex-1 truncate text-sm font-medium">
            {title}
          </Span>

          <Badge variant={badge.variant} size="sm">
            {badge.label}
          </Badge>

          {duration && (
            <Span variant="muted" className="shrink-0 text-xs tabular-nums">
              {duration}
            </Span>
          )}

          {href && (
            <a
              href={href}
              target="_blank"
              rel="noopener noreferrer"
              className="text-primary dark:text-dark-primary hover:text-primary-600 dark:hover:text-dark-primary-400 shrink-0"
              aria-label="Open in external tool"
            >
              <ExternalLink className="h-3.5 w-3.5" />
            </a>
          )}

          {detail && (
            <button
              type="button"
              onClick={() => setOpen((v) => !v)}
              className={cn(
                "shrink-0 rounded p-0.5",
                "text-text-muted dark:text-dark-text-muted",
                "hover:bg-surface-100 dark:hover:bg-dark-surface-300",
              )}
              aria-expanded={open}
              aria-label="Toggle detail"
            >
              <ChevronDown
                className={cn(
                  "h-4 w-4 transition-transform",
                  open && "rotate-180",
                )}
              />
            </button>
          )}
        </div>

        {/* Collapsible detail */}
        {detail && open && (
          <div
            className={cn(
              "border-t px-3 py-2",
              "border-surface-200 dark:border-dark-surface-500",
              "bg-surface-100 dark:bg-dark-surface-300",
              "overflow-auto text-xs font-mono",
              "text-text-muted dark:text-dark-text-muted",
              "max-h-60",
            )}
          >
            {detail}
          </div>
        )}
      </div>
    );
  },
);
