import { forwardRef } from "react";
import { cn } from "../utils";

export type InlineNoticeVariant = "warning" | "info" | "error" | "errorSoft";

export type InlineNoticeProps = React.HTMLAttributes<HTMLDivElement> & {
  variant?: InlineNoticeVariant;
};

const variantClasses: Record<InlineNoticeVariant, string> = {
  warning:
    "bg-warning-50 dark:bg-dark-surface-300 text-warning-900 dark:text-dark-text border-warning-200 dark:border-dark-surface-500 shrink-0 border-b px-3 py-1.5 text-xs",
  info: "border-primary-200 bg-primary-50 text-text dark:bg-dark-surface-200 dark:text-dark-text shrink-0 rounded-lg border px-3 py-2 text-sm whitespace-pre-wrap",
  error:
    "bg-error-500 dark:bg-dark-error-100 text-error-800 dark:text-dark-text absolute top-4 right-4 left-4 z-10 shrink-0 rounded-lg p-4 shadow-sm",
  errorSoft:
    "bg-error-50 dark:bg-dark-error-400 text-error-800 dark:text-dark-text shrink-0 rounded-lg border border-error-200 p-4 dark:border-dark-surface-600",
};

export const InlineNotice = forwardRef<HTMLDivElement, InlineNoticeProps>(function InlineNotice(
  { className, variant = "info", ...props },
  ref,
) {
  return <div ref={ref} className={cn(variantClasses[variant], className)} {...props} />;
});
