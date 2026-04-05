// components/Inbox.tsx
import { forwardRef } from "react";
import { cn } from "../utils";

type InboxProps = React.InputHTMLAttributes<HTMLInputElement>;

export const Inbox = forwardRef<HTMLInputElement, InboxProps>(
  ({ className, ...props }, ref) => {
    return (
      <input
        ref={ref}
        className={cn(
          "border-secondary-300 bg-surface-50 text-text w-full rounded-lg border px-4 py-2.5",
          "focus:ring-primary-500 focus:border-transparent focus:ring-2 focus:ring-offset-2",
          "dark:border-dark-secondary-300 dark:bg-dark-surface-50 dark:text-dark-text dark:focus:ring-dark-primary-500",
          className,
        )}
        {...props}
      />
    );
  },
);
Inbox.displayName = "Inbox";
