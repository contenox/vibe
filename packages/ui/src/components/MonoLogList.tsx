import { forwardRef } from "react";
import { cn } from "../utils";

export type MonoLogListProps = React.HTMLAttributes<HTMLUListElement> & {
  /** Scroll container max height (default max-h-48). */
  maxHeightClassName?: string;
};

export const MonoLogList = forwardRef<HTMLUListElement, MonoLogListProps>(function MonoLogList(
  { className, maxHeightClassName = "max-h-48", ...props },
  ref,
) {
  return (
    <ul
      ref={ref}
      className={cn(
        "space-y-1 overflow-y-auto border border-dashed border-surface-300 px-2 py-1.5 font-mono text-[10px] dark:border-dark-surface-600",
        maxHeightClassName,
        className,
      )}
      {...props}
    />
  );
});

export type MonoLogListItemProps = React.LiHTMLAttributes<HTMLLIElement>;

export const MonoLogListItem = forwardRef<HTMLLIElement, MonoLogListItemProps>(
  function MonoLogListItem({ className, ...props }, ref) {
    return (
      <li
        ref={ref}
        className={cn(
          "text-text dark:text-dark-text border-surface-200 border-b border-dotted pb-0.5 last:border-0 dark:border-dark-surface-600",
          className,
        )}
        {...props}
      />
    );
  },
);
