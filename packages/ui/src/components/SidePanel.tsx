import { forwardRef } from "react";
import { cn } from "../utils";

/** Fixed-width secondary column (e.g. run log) with left border. */
export type SidePanelColumnProps = React.HTMLAttributes<HTMLDivElement> & {
  side?: "left" | "right";
};

export const SidePanelColumn = forwardRef<HTMLDivElement, SidePanelColumnProps>(
  function SidePanelColumn({ className, side = "right", ...props }, ref) {
    return (
      <div
        ref={ref}
        className={cn(
          "bg-surface-50 dark:bg-dark-surface-200 flex w-[min(100%,20rem)] flex-shrink-0 flex-col sm:w-80",
          side === "right" ? "border-l" : "border-r",
          className,
        )}
        {...props}
      />
    );
  },
);

export type SidePanelHeaderProps = React.HTMLAttributes<HTMLDivElement>;

export const SidePanelHeader = forwardRef<HTMLDivElement, SidePanelHeaderProps>(
  function SidePanelHeader({ className, ...props }, ref) {
    return (
      <div
        ref={ref}
        className={cn("flex shrink-0 items-center justify-between gap-2 border-b px-2 py-2", className)}
        {...props}
      />
    );
  },
);

export type SidePanelBodyProps = React.HTMLAttributes<HTMLDivElement>;

export const SidePanelBody = forwardRef<HTMLDivElement, SidePanelBodyProps>(
  function SidePanelBody({ className, ...props }, ref) {
    return (
      <div
        ref={ref}
        className={cn("flex min-h-0 flex-1 flex-col gap-2 overflow-auto p-2", className)}
        {...props}
      />
    );
  },
);

export type SidePanelRailButtonProps = React.ButtonHTMLAttributes<HTMLButtonElement> & {
  side?: "left" | "right";
};

/** Narrow strip shown when the side panel is collapsed (icon + optional badge). */
export const SidePanelRailButton = forwardRef<HTMLButtonElement, SidePanelRailButtonProps>(
  function SidePanelRailButton({ className, side = "right", type = "button", ...props }, ref) {
    return (
      <button
        ref={ref}
        type={type}
        className={cn(
          "bg-surface-50 dark:bg-dark-surface-200 text-secondary-600 hover:bg-surface-100 dark:text-dark-secondary-400 dark:hover:bg-dark-surface-300 flex w-9 shrink-0 flex-col items-center justify-center",
          side === "right" ? "border-l" : "border-r",
          className,
        )}
        {...props}
      />
    );
  },
);
