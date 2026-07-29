import { cn } from "../../utils";

export type ChatDateSeparatorProps = {
  label: string;
  className?: string;
};

export function ChatDateSeparator({
  label,
  className,
}: ChatDateSeparatorProps) {
  return (
    <div
      className={cn("flex items-center gap-3 py-2", className)}
      role="separator"
      aria-label={label}
    >
      <div className="bg-surface-300 dark:bg-dark-surface-600 h-px flex-1" />
      <span className="text-text-muted dark:text-dark-text-muted shrink-0 text-xs font-medium">
        {label}
      </span>
      <div className="bg-surface-300 dark:bg-dark-surface-600 h-px flex-1" />
    </div>
  );
}
