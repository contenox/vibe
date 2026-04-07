import React, { forwardRef } from "react";
import { cn } from "../utils";

type TabTriggerProps = React.ButtonHTMLAttributes<HTMLButtonElement> & {
  active: boolean;
};

export const TabTrigger = forwardRef<HTMLButtonElement, TabTriggerProps>(
  ({ active, className, children, ...props }, ref) => (
    <button
      ref={ref}
      role="tab"
      aria-selected={active}
      type="button"
      {...props}
      className={cn(
        "group relative px-5 py-2.5 rounded-none transition-colors",
        "focus:outline-none focus-visible:ring-1 focus-visible:ring-primary/20",
        "hover:bg-white/5",
        active
          ? "text-primary-500 dark:text-dark-primary-400 after:scale-x-100"
          : "text-foreground/80 after:scale-x-0 group-hover:after:scale-x-100",
        "after:absolute after:inset-x-3 after:bottom-0 after:h-px after:bg-primary-500/60 dark:after:bg-dark-primary-400/60 after:transition-transform after:origin-left",
        className,
      )}
    >
      {children}
    </button>
  ),
);
TabTrigger.displayName = "TabTrigger";
