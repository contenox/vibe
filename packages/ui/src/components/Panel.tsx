import { forwardRef } from "react";
import { cn } from "../utils";

interface PanelProps extends React.HTMLAttributes<HTMLDivElement> {
  variant?:
    | "default"
    | "raised"
    | "flat"
    | "bordered"
    | "error"
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
          "p-4 m-2 inherit bg-inherit text-inherit": variant === "default",
          "p-4 m-2 shadow-sm dark:shadow-md": variant === "raised",
          "p-4 m-2 border border-surface-300 dark:border-dark-surface-700":
            variant === "bordered",
          "p-0 m-0 border-0 shadow-none": variant === "flat",
          "p-4 m-2bg-error-50 dark:bg-dark-error-900 text-error dark:text-dark-error":
            variant === "error",
          "p-4 m-2 bg-gradient-to-br from-primary-600 to-accent-700 !text-white":
            variant === "gradient",
          "p-4 m-2 bg-surface-50 dark:bg-dark-surface-100 border border-surface-200 dark:border-dark-surface-700":
            variant === "surface",
          "p-4 m-2 bg-transparent hover:bg-surface-50 dark:hover:bg-dark-surface-800 border border-surface-100 dark:border-dark-surface-700":
            variant === "ghost",
          "p-4 -mx-2 border-t border-[var(--color-surface-300)] dark:border-[var(--color-dark-surface-700)]":
            variant === "body",
          "": variant === "empty",
        },
        className,
      )}
      {...props}
    />
  ),
);
