import { forwardRef } from "react";
import { cn } from "../utils";

export type InlineNoticeVariant = "warning" | "info" | "error" | "errorSoft";

export type InlineNoticeProps = React.HTMLAttributes<HTMLDivElement> & {
  variant?: InlineNoticeVariant;
  onDismiss?: () => void;
};

const variantClasses: Record<InlineNoticeVariant, string> = {
  warning:
    "bg-warning-50 dark:bg-dark-surface-300 text-warning-900 dark:text-dark-text border-warning-200 dark:border-dark-surface-500 shrink-0 border-b px-3 py-1.5 text-xs",
  info: "border-surface-300 bg-surface-100 text-text dark:border-dark-surface-500 dark:bg-dark-surface-200 dark:text-dark-text shrink-0 rounded-lg border px-3 py-2 text-sm whitespace-pre-wrap",
  error:
    "bg-error-50 dark:bg-dark-error-100 text-error-800 dark:text-dark-text border-error-200 dark:border-dark-surface-500 shrink-0 rounded-lg border px-3 py-2 text-sm",
  errorSoft:
    "bg-error-50 dark:bg-dark-error-400 text-error-800 dark:text-dark-text shrink-0 rounded-lg border border-error-200 p-4 dark:border-dark-surface-600",
};

export const InlineNotice = forwardRef<HTMLDivElement, InlineNoticeProps>(function InlineNotice(
  { className, variant = "info", onDismiss, children, ...props },
  ref,
) {
  return (
    <div ref={ref} className={cn(variantClasses[variant], className)} {...props}>
      {onDismiss ? (
        <div className="flex items-center justify-between gap-2">
          <div className="min-w-0 flex-1">{children}</div>
          <button
            type="button"
            onClick={onDismiss}
            className="text-current opacity-60 hover:opacity-100 shrink-0 px-1 text-lg leading-none"
            aria-label="Dismiss"
          >
            &times;
          </button>
        </div>
      ) : (
        children
      )}
    </div>
  );
});
