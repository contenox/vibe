import { forwardRef } from "react";
import { cn } from "../utils";

export type InsetPanelTone = "default" | "muted" | "strip" | "section";

export type InsetPanelProps = React.HTMLAttributes<HTMLDivElement> & {
  /** default: nested workbench card. muted: elevated surface (errors, loading). strip: full-width row with bottom border. section: scroll bucket under toolbar (border-b, surface-100). */
  tone?: InsetPanelTone;
};

export const InsetPanel = forwardRef<HTMLDivElement, InsetPanelProps>(function InsetPanel(
  { className, tone = "default", ...props },
  ref,
) {
  return (
    <div
      ref={ref}
      className={cn(
        "border-border",
        tone === "default" &&
          "bg-surface-50 dark:bg-dark-surface-200 flex min-h-0 flex-col overflow-hidden rounded-lg border",
        tone === "muted" && "bg-surface-100 dark:bg-dark-surface-300 rounded-lg border",
        tone === "strip" && "bg-surface-100 dark:bg-dark-surface-300 flex shrink-0 flex-col border-b",
        tone === "section" &&
          "bg-surface-100 dark:bg-dark-surface-300 flex min-h-0 shrink-0 flex-col border-b",
        className,
      )}
      {...props}
    />
  );
});

export type InsetPanelHeaderProps = React.HTMLAttributes<HTMLDivElement> & {
  /** compact: py-1.5 (graph titles). comfortable: py-2 (captions, toolbars). */
  density?: "compact" | "comfortable";
};

export const InsetPanelHeader = forwardRef<HTMLDivElement, InsetPanelHeaderProps>(
  function InsetPanelHeader({ className, density = "compact", ...props }, ref) {
    return (
      <div
        ref={ref}
        className={cn(
          "border-border shrink-0 border-b px-3",
          density === "compact" ? "py-1.5" : "py-2",
          className,
        )}
        {...props}
      />
    );
  },
);

export type InsetPanelBodyProps = React.HTMLAttributes<HTMLDivElement>;

export const InsetPanelBody = forwardRef<HTMLDivElement, InsetPanelBodyProps>(
  function InsetPanelBody({ className, ...props }, ref) {
    return (
      <div
        ref={ref}
        className={cn("min-h-0 flex-1 overflow-hidden px-2 pb-2", className)}
        {...props}
      />
    );
  },
);
