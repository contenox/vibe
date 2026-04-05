import React, { forwardRef } from "react";
import { cn } from "../utils";

type CardLayout = "default" | "space-between";

type CardProps = React.HTMLAttributes<HTMLDivElement> & {
  variant?: "default" | "filled" | "surface" | "error" | "bordered" | "dotted";
  layout?: CardLayout;
};

export const Card = forwardRef<HTMLDivElement, CardProps>(
  ({ className, variant = "default", layout = "default", ...props }, ref) => {
    const dottedPatternClasses =
      "[--dot-size:1px] [--dot-gap:18px] " +
      "[--dot-color:rgba(0,0,0,0.14)] dark:[--dot-color:rgba(255,255,255,0.14)] " +
      "[background-image:radial-gradient(var(--dot-color)_var(--dot-size),transparent_var(--dot-size))] " +
      "[background-size:var(--dot-gap)_var(--dot-gap)] " +
      "bg-surface-100 dark:bg-dark-surface-700 " +
      "border-surface-300 dark:border-dark-surface-600";

    return (
      <div
        ref={ref}
        className={cn(
          "rounded-xl border p-6 m-2 shadow-sm transition-colors",
          "dark:border-dark-surface-700",
          {
            "bg-surface-50 border-surface-200 dark:bg-dark-surface-800":
              variant === "default",
            "bg-secondary-100 border-secondary-200 dark:bg-dark-surface-600":
              variant === "filled",
            "bg-surface-100 border-surface-300 dark:bg-dark-surface-900 dark:border-dark-surface-600":
              variant === "surface",
            "bg-error-50 dark:bg-dark-error-900 text-error dark:text-dark-error":
              variant === "error",
            "border border-surface-400 dark:border-dark-surface-500":
              variant === "bordered",
            [dottedPatternClasses]: variant === "dotted",
          },
          {
            "flex items-center justify-between": layout === "space-between",
          },
          className,
        )}
        {...props}
      />
    );
  },
);

Card.displayName = "Card";
