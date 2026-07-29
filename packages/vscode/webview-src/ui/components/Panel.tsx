import { forwardRef } from "react";
import { cn } from "../utils";

interface PanelProps extends React.HTMLAttributes<HTMLDivElement> {
  variant?:
    | "default"
    | "raised"
    | "flat"
    | "bordered"
    | "error"
    | "warning"
    | "info"
    | "gradient"
    | "surface"
    | "ghost"
    | "empty"
    | "body";
}

export const Panel = forwardRef<HTMLDivElement, PanelProps>(
  ({ className, variant = "default", ...props }, ref) => (
    <div
      ref={ref}
      className={cn(
        // Base styles
        "transition-colors",
        // Conditionally remove rounded corners for the topBordered variant
        variant === "body" ? "rounded-none" : "rounded-lg",
        {
          // Variants
          "p-4 inherit bg-inherit text-inherit": variant === "default",
          "p-4 shadow-sm dark:shadow-md": variant === "raised",
          "p-4 border border-surface-300 dark:border-dark-surface-700":
            variant === "bordered",
          "p-0 border-0 shadow-none": variant === "flat",
          "p-4 bg-error-50 dark:bg-dark-error-100 text-error dark:text-dark-error-700 border border-error-200 dark:border-dark-error-300":
            variant === "error",
          "p-4 bg-warning-50 dark:bg-dark-warning-50 text-warning-900 dark:text-dark-warning-800 border border-warning-200 dark:border-dark-warning-200":
            variant === "warning",
          "p-4 bg-info-50 dark:bg-dark-surface-200 text-info-900 dark:text-dark-text border border-info-200 dark:border-dark-surface-500":
            variant === "info",
          "p-4 bg-gradient-to-br from-primary-600 to-accent-700 text-text-inverted dark:from-dark-primary-500 dark:to-dark-primary-700 dark:text-dark-text-inverted":
            variant === "gradient",
          "p-4 bg-surface-50 dark:bg-dark-surface-100 border border-surface-200 dark:border-dark-surface-700":
            variant === "surface",
          "p-4 bg-transparent hover:bg-surface-50 dark:hover:bg-dark-surface-800 border border-surface-100 dark:border-dark-surface-700":
            variant === "ghost",
          "p-4 border-t border-[var(--color-surface-300)] dark:border-[var(--color-dark-surface-700)]":
            variant === "body",
          "": variant === "empty",
        },
        className,
      )}
      {...props}
    />
  ),
);
